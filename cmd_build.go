// +build !lambdabinary

package sparta

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/iam"
	spartaAWS "github.com/mweagle/Sparta/aws"
	"github.com/mweagle/Sparta/system"
	spartaZip "github.com/mweagle/Sparta/zip"
	gocc "github.com/mweagle/go-cloudcondenser"
	gocf "github.com/mweagle/go-cloudformation"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
)

// userdata is user-supplied, code related values
type userdata struct {
	// Is this is a -dry-run?
	noop bool
	// Is this a CGO enabled build?
	useCGO bool
	// To do in-place updates we'd need to find all the function ARNs
	// and then update them. That requires the names to be stable. Interesting.
	// Are in-place updates enabled?
	inPlace bool
	// The user-supplied or automatically generated BuildID
	buildID string
	// Optional user-supplied build tags
	buildTags string
	// Optional link flags
	linkFlags string
	// Canonical basename of the service.  Also used as the CloudFormation
	// stack name
	serviceName string
	// Service description
	serviceDescription string
	// The slice of Lambda functions that constitute the service
	lambdaAWSInfos []*LambdaAWSInfo
	// User supplied workflow hooks
	workflowHooks *WorkflowHooks
	// Code pipeline S3 trigger keyname
	codePipelineTrigger string
	// Optional APIGateway definition to associate with this service
	api APIGateway
	// Optional S3 site data to provision together with this service
	s3SiteContext *s3SiteContext
	// The user-supplied S3 bucket where service artifacts should be posted.
	s3Bucket string
}

// context is data that is mutated during the building workflow
type buildContext struct {
	// Output location for files
	outputDirectory string
	// AWS Session to be used for all API calls made in the process of provisioning
	// this service.
	awsSession *session.Session
	// Cached IAM role name map.  Used to support dynamic and static IAM role
	// names.  Static ARN role names are checked for existence via AWS APIs
	// prior to CloudFormation provisioning.
	lambdaIAMRoleNameMap map[string]*gocf.StringExpr
	// IO writer for autogenerated template results
	templateWriter io.Writer
	// CloudFormation Template
	cfTemplate *gocf.Template
	// name of the binary inside the ZIP archive
	compiledBinaryOutput string
	// Context to pass between workflow operations
	workflowHooksContext context.Context
}

// Encapsulate calling the archive hooks
func callArchiveHook(lambdaArchive *zip.Writer,
	userdata *userdata,
	buildContext *buildContext,
	logger *zerolog.Logger) error {

	if userdata.workflowHooks == nil {
		return nil
	}
	for _, eachArchiveHook := range userdata.workflowHooks.Archives {
		// Run the hook
		logger.Info().
			Interface("WorkflowHookContext", buildContext.workflowHooksContext).
			Msg("Calling ArchiveHook")

		hookCtx, hookErr := eachArchiveHook.DecorateArchive(buildContext.workflowHooksContext,
			userdata.serviceName,
			lambdaArchive,
			buildContext.awsSession,
			userdata.noop,
			logger)
		if hookErr != nil {
			return errors.Wrapf(hookErr, "DecorateArchive returned an error")
		}
		buildContext.workflowHooksContext = hookCtx
	}
	return nil
}

// create a new gocf.Parameter struct and return it...
func newStackParameter(paramType string,
	description string,
	defaultValue string,
	allowedPattern string,
	minLength int64) *gocf.Parameter {

	return &gocf.Parameter{
		Type:           paramType,
		Description:    description,
		Default:        defaultValue,
		AllowedPattern: allowedPattern,
		MinLength:      gocf.Integer(minLength),
	}
}

// Encapsulate calling a workflow hook
func callWorkflowHook(hookPhase string,
	hooks []WorkflowHookHandler,
	userdata *userdata,
	buildContext *buildContext,
	logger *zerolog.Logger) error {

	for _, eachHook := range hooks {
		// Run the hook
		logger.Info().
			Str("Phase", hookPhase).
			Interface("WorkflowHookContext", buildContext.workflowHooksContext).
			Msg("Calling WorkflowHook")

		hookCtx, hookErr := eachHook.DecorateWorkflow(buildContext.workflowHooksContext,
			userdata.serviceName,
			gocf.Ref(StackParamS3CodeBucketName),
			userdata.buildID,
			buildContext.awsSession,
			userdata.noop,
			logger)
		if hookErr != nil {
			return errors.Wrapf(hookErr, "DecorateWorkflow returned an error")
		}
		buildContext.workflowHooksContext = hookCtx
	}
	return nil
}

// Encapsulate calling the service decorator hooks
func callServiceDecoratorHook(lambdaFunctionCode *gocf.LambdaFunctionCode,
	userdata *userdata,
	buildContext *buildContext,
	logger *zerolog.Logger) error {
	if userdata.workflowHooks == nil {
		return nil
	}
	// If there's an API gateway definition, include the resources that provision it.
	// Since this export will likely
	// generate outputs that the s3 site needs, we'll use a temporary outputs accumulator,
	// pass that to the S3Site
	// if it's defined, and then merge it with the normal output map.-
	for eachIndex, eachServiceHook := range userdata.workflowHooks.ServiceDecorators {
		funcPtr := reflect.ValueOf(eachServiceHook).Pointer()
		funcForPC := runtime.FuncForPC(funcPtr)
		hookName := funcForPC.Name()
		if hookName == "" {
			hookName = fmt.Sprintf("ServiceHook[%d]", eachIndex)
		}
		logger.Info().
			Str("ServiceDecoratorHook", hookName).
			Interface("WorkflowHookContext", buildContext.workflowHooksContext).
			Msg("Calling WorkflowHook")

		serviceTemplate := gocf.NewTemplate()
		decoratorCtx, decoratorError := eachServiceHook.DecorateService(buildContext.workflowHooksContext,
			userdata.serviceName,
			serviceTemplate,
			lambdaFunctionCode,
			userdata.buildID,
			buildContext.awsSession,
			userdata.noop,
			logger)
		if nil != decoratorError {
			return decoratorError
		}
		buildContext.workflowHooksContext = decoratorCtx
		safeMergeErrs := gocc.SafeMerge(serviceTemplate, buildContext.cfTemplate)
		if len(safeMergeErrs) != 0 {
			return errors.Errorf("Failed to merge templates: %#v", safeMergeErrs)
		}
	}
	return nil
}

// Encapsulate calling the validation hooks
func callValidationHooks(validationHooks []ServiceValidationHookHandler,
	template *gocf.Template,
	lambdaFunctionCode *gocf.LambdaFunctionCode,
	userdata *userdata,
	buildContext *buildContext,
	logger *zerolog.Logger) error {

	var marshaledTemplate []byte
	if len(validationHooks) != 0 {
		jsonBytes, jsonBytesErr := json.Marshal(template)
		if jsonBytesErr != nil {
			return errors.Wrapf(jsonBytesErr, "Failed to marshal template for validation")
		}
		marshaledTemplate = jsonBytes
	}

	for _, eachHook := range validationHooks {
		// Run the hook
		logger.Info().
			Str("Phase", "Validation").
			Interface("ValidationHookContext", buildContext.workflowHooksContext).
			Msg("Calling WorkflowHook")

		var loopTemplate gocf.Template
		unmarshalErr := json.Unmarshal(marshaledTemplate, &loopTemplate)
		if unmarshalErr != nil {
			return errors.Wrapf(unmarshalErr,
				"Failed to unmarshal read-only copy of template for Validation")
		}

		hookCtx, hookErr := eachHook.ValidateService(buildContext.workflowHooksContext,
			userdata.serviceName,
			&loopTemplate,
			lambdaFunctionCode,
			userdata.buildID,
			buildContext.awsSession,
			userdata.noop,
			logger)
		if hookErr != nil {
			return errors.Wrapf(hookErr, "Service failed to pass validation")
		}
		buildContext.workflowHooksContext = hookCtx
	}
	return nil
}

////////////////////////////////////////////////////////////////////////////////
//
// BEGIN - Private
//

type verifyAWSPreconditionsOp struct {
	userdata *userdata
}

func (vapo *verifyAWSPreconditionsOp) Invoke(ctx context.Context, logger *zerolog.Logger) error {
	// If there are codePipeline environments defined, warn if they don't include
	// the same keysets
	if nil != codePipelineEnvironments {
		mapKeys := func(inboundMap map[string]string) []string {
			keys := make([]string, len(inboundMap))
			i := 0
			for k := range inboundMap {
				keys[i] = k
				i++
			}
			return keys
		}
		aggregatedKeys := make([][]string, len(codePipelineEnvironments))
		i := 0
		for _, eachEnvMap := range codePipelineEnvironments {
			aggregatedKeys[i] = mapKeys(eachEnvMap)
			i++
		}
		i = 0
		keysEqual := true
		for _, eachKeySet := range aggregatedKeys {
			j := 0
			for _, eachKeySetTest := range aggregatedKeys {
				if j != i {
					if !reflect.DeepEqual(eachKeySet, eachKeySetTest) {
						keysEqual = false
					}
				}
				j++
			}
			i++
		}
		if !keysEqual {
			// Setup an interface with the fields so that the log message
			logEntry := logger.Warn()
			for eachEnv, eachEnvMap := range codePipelineEnvironments {
				logEntry = logEntry.Interface(eachEnv, eachEnvMap)
			}
			logEntry.Msg("CodePipeline environments do not define equivalent environment keys")
		}
	}
	return nil
}
func (vapo *verifyAWSPreconditionsOp) Rollback(ctx context.Context, logger *zerolog.Logger) error {
	return nil
}

type validatePreconditionsOp struct {
	userdata *userdata
}

func (vpo *validatePreconditionsOp) Rollback(ctx context.Context, logger *zerolog.Logger) error {
	return nil
}

func (vpo *validatePreconditionsOp) Invoke(ctx context.Context, logger *zerolog.Logger) error {
	var errorText []string
	collisionMemo := make(map[string]int)

	incrementCounter := func(keyName string) {
		_, exists := collisionMemo[keyName]
		if !exists {
			collisionMemo[keyName] = 1
		} else {
			collisionMemo[keyName] = collisionMemo[keyName] + 1
		}
	}
	// 00 - check for possibly empty function set...
	if len(vpo.userdata.lambdaAWSInfos) <= 0 {
		// Warning? Maybe it's just decorators?
		if vpo.userdata.workflowHooks == nil {
			return errors.New("No lambda functions were provided to Sparta.Provision(). WorkflowHooks are undefined")
		}
		logger.Warn().Msg("No lambda functions provided to Sparta.Provision()")
	}

	// 0 - check for nil
	for eachIndex, eachLambda := range vpo.userdata.lambdaAWSInfos {
		if eachLambda == nil {
			errorText = append(errorText,
				fmt.Sprintf("Lambda at position %d is `nil`", eachIndex))
		}
	}
	// Semantic checks only iff lambdas are non-nil
	if len(errorText) == 0 {

		// 1 - check for invalid signatures
		for _, eachLambda := range vpo.userdata.lambdaAWSInfos {
			validationErr := ensureValidSignature(eachLambda.userSuppliedFunctionName,
				eachLambda.handlerSymbol)
			if validationErr != nil {
				errorText = append(errorText, validationErr.Error())
			}
		}

		// 2 - check for duplicate golang function references.
		for _, eachLambda := range vpo.userdata.lambdaAWSInfos {
			incrementCounter(eachLambda.lambdaFunctionName())
			for _, eachCustom := range eachLambda.customResources {
				incrementCounter(eachCustom.userFunctionName)
			}
		}
		// Duplicates?
		for eachLambdaName, eachCount := range collisionMemo {
			if eachCount > 1 {
				logger.Error().
					Int("CollisionCount", eachCount).
					Str("Name", eachLambdaName).
					Msg("NewAWSLambda")
				errorText = append(errorText,
					fmt.Sprintf("Multiple definitions of lambda: %s", eachLambdaName))
			}
		}
		logger.Debug().
			Interface("CollisionMap", collisionMemo).
			Msg("Lambda collision map")
	}
	if len(errorText) != 0 {
		return errors.New(strings.Join(errorText[:], "\n"))
	}
	return nil
}

type verifyIAMRolesOp struct {
	userdata     *userdata
	buildContext *buildContext
}

func (viro *verifyIAMRolesOp) Invoke(ctx context.Context, logger *zerolog.Logger) error {
	// The map is either a literal Arn from a pre-existing role name
	// or a gocf.RefFunc() value.
	// Don't verify them, just create them...
	viro.buildContext.lambdaIAMRoleNameMap = make(map[string]*gocf.StringExpr)
	iamSvc := iam.New(viro.buildContext.awsSession)

	// Assemble all the RoleNames and validate the inline IAMRoleDefinitions
	var allRoleNames []string
	for _, eachLambdaInfo := range viro.userdata.lambdaAWSInfos {
		if eachLambdaInfo.RoleName != "" {
			allRoleNames = append(allRoleNames, eachLambdaInfo.RoleName)
		}
		// Custom resources?
		for _, eachCustomResource := range eachLambdaInfo.customResources {
			if eachCustomResource.roleName != "" {
				allRoleNames = append(allRoleNames, eachCustomResource.roleName)
			}
		}
		// Profiling enabled?
		if profileDecorator != nil {
			profileErr := profileDecorator(viro.userdata.serviceName,
				eachLambdaInfo,
				viro.userdata.s3Bucket,
				logger)
			if profileErr != nil {
				return errors.Wrapf(profileErr, "Failed to call lambda profile decorator")
			}
		}

		// Validate the IAMRoleDefinitions associated
		if nil != eachLambdaInfo.RoleDefinition {
			logicalName := eachLambdaInfo.RoleDefinition.logicalName(viro.userdata.serviceName,
				eachLambdaInfo.lambdaFunctionName())
			_, exists := viro.buildContext.lambdaIAMRoleNameMap[logicalName]
			if !exists {
				// Insert it into the resource creation map and add
				// the "Ref" entry to the hashmap
				viro.buildContext.cfTemplate.AddResource(logicalName,
					eachLambdaInfo.RoleDefinition.toResource(eachLambdaInfo.EventSourceMappings,
						eachLambdaInfo.Options,
						logger))

				viro.buildContext.lambdaIAMRoleNameMap[logicalName] = gocf.GetAtt(logicalName, "Arn")
			}
		}

		// And the custom resource IAMRoles as well...
		for _, eachCustomResource := range eachLambdaInfo.customResources {
			if nil != eachCustomResource.roleDefinition {
				customResourceLogicalName := eachCustomResource.roleDefinition.logicalName(viro.userdata.serviceName,
					eachCustomResource.userFunctionName)

				_, exists := viro.buildContext.lambdaIAMRoleNameMap[customResourceLogicalName]
				if !exists {
					viro.buildContext.cfTemplate.AddResource(customResourceLogicalName,
						eachCustomResource.roleDefinition.toResource(nil,
							eachCustomResource.options,
							logger))
					viro.buildContext.lambdaIAMRoleNameMap[customResourceLogicalName] = gocf.GetAtt(customResourceLogicalName, "Arn")
				}
			}
		}
	}

	// Then check all the RoleName literals
	totalRemoteChecks := 0
	for _, eachRoleName := range allRoleNames {
		_, exists := viro.buildContext.lambdaIAMRoleNameMap[eachRoleName]
		if !exists {
			totalRemoteChecks++
			// Check the role
			params := &iam.GetRoleInput{
				RoleName: aws.String(eachRoleName),
			}

			logger.Debug().Msgf("Checking IAM RoleName: %s", eachRoleName)
			resp, err := iamSvc.GetRole(params)
			if err != nil {
				return err
			}
			// Cache it - we'll need it later when we create the
			// CloudFormation template which needs the execution Arn (not role)
			viro.buildContext.lambdaIAMRoleNameMap[eachRoleName] = gocf.String(*resp.Role.Arn)
		}
	}

	logger.Info().
		Int("GetRoleCount", totalRemoteChecks).
		Int("Total", len(viro.buildContext.lambdaIAMRoleNameMap)).
		Msg("Verified IAM Roles")
	return nil
}

func (viro *verifyIAMRolesOp) Rollback(ctx context.Context, logger *zerolog.Logger) error {
	return nil
}

//
// END - Private
//
////////////////////////////////////////////////////////////////////////////////

type createPackageOp struct {
	userdata     *userdata
	buildContext *buildContext
}

func (cpo *createPackageOp) Rollback(ctx context.Context, logger *zerolog.Logger) error {
	return nil
}

func (cpo *createPackageOp) Invoke(ctx context.Context, logger *zerolog.Logger) error {

	// PreBuild Hook
	if cpo.userdata.workflowHooks != nil {
		preBuildErr := callWorkflowHook("PreBuild",
			cpo.userdata.workflowHooks.PreBuilds,
			cpo.userdata,
			cpo.buildContext,
			logger)
		if nil != preBuildErr {
			return preBuildErr
		}
	}
	sanitizedServiceName := sanitizedName(cpo.userdata.serviceName)
	// Output location
	buildErr := system.BuildGoBinary(cpo.userdata.serviceName,
		cpo.buildContext.compiledBinaryOutput,
		cpo.userdata.useCGO,
		cpo.userdata.buildID,
		cpo.userdata.buildTags,
		cpo.userdata.linkFlags,
		cpo.userdata.noop,
		logger)
	if nil != buildErr {
		return buildErr
	}

	//////////////////////////////////////////////////////////////////////////////
	// Build the Site ZIP?
	// We might need to upload some other things...
	if nil != cpo.userdata.s3SiteContext.s3Site {
		absResourcePath, err := filepath.Abs(cpo.userdata.s3SiteContext.s3Site.resources)
		if nil != err {
			return errors.Wrapf(err,
				"Failed to get absolute filepath for S3 Site contents directory (%s)",
				cpo.userdata.s3SiteContext.s3Site.resources)
		}
		// Ensure that the directory exists...
		_, existsErr := os.Stat(cpo.userdata.s3SiteContext.s3Site.resources)
		if existsErr != nil && os.IsNotExist(existsErr) {
			return errors.Wrapf(existsErr,
				"The S3 Site resources directory (%s) does not exist",
				cpo.userdata.s3SiteContext.s3Site.resources)
		}

		// Create ZIP output archive
		siteZIPArchiveName := fmt.Sprintf("%s-S3Site.zip", cpo.userdata.serviceName)
		siteZIPArchivePath := filepath.Join(cpo.buildContext.outputDirectory, siteZIPArchiveName)
		zipOutputFile, zipOutputFileErr := os.Create(siteZIPArchivePath)
		if zipOutputFileErr != nil {
			return errors.Wrapf(zipOutputFileErr, "Failed to create temporary S3 site archive file")
		}
		// Add the contents to the Zip file
		zipArchive := zip.NewWriter(zipOutputFile)
		addToZipErr := spartaZip.AddToZip(zipArchive,
			absResourcePath,
			absResourcePath,
			logger)
		if addToZipErr != nil {
			return errors.Wrapf(addToZipErr, "Failed to create S3 site ZIP archive")
		}
		// Else, save it...
		cpo.buildContext.cfTemplate.Metadata[MetadataParamS3SiteArchivePath] = zipOutputFile.Name()

		archiveCloseErr := zipArchive.Close()
		if nil != archiveCloseErr {
			return errors.Wrapf(archiveCloseErr, "Failed to close S3 site ZIP stream")
		}
		zipCloseErr := zipOutputFile.Close()
		if zipCloseErr != nil {
			return errors.Wrapf(zipCloseErr, "Failed to close S3 site ZIP archive")
		}
		logger.Info().
			Str("Path", zipOutputFile.Name()).
			Msg("Created S3Site archive")

		// Put the path into the template metadata, include the stack parameter...
		cpo.buildContext.cfTemplate.Metadata[MetadataParamS3SiteArchivePath] = zipOutputFile.Name()

		// Add this as a stack param. By default it's going to have
		// the same keyname as the file we just created...
		cpo.buildContext.cfTemplate.Parameters[StackParamS3SiteArchiveKey] = newStackParameter(
			"String",
			"Object key that stores the S3 site archive.",
			filepath.Base(zipOutputFile.Name()),
			".+",
			3)
		cpo.buildContext.cfTemplate.Parameters[StackParamS3SiteArchiveVersion] = newStackParameter(
			"String",
			"Object version of the S3 archive.",
			"",
			"",
			0)
	}

	// PostBuild Hook
	if cpo.userdata.workflowHooks != nil {
		postBuildErr := callWorkflowHook("PostBuild",
			cpo.userdata.workflowHooks.PostBuilds,
			cpo.userdata,
			cpo.buildContext,
			logger)
		if nil != postBuildErr {
			return postBuildErr
		}
	}
	//////////////////////////////////////////////////////////////////////////////
	// Build the code ZIP
	codeArchiveName := fmt.Sprintf("%s-code.zip", sanitizedServiceName)
	codeZIPArchivePath := filepath.Join(cpo.buildContext.outputDirectory, codeArchiveName)
	zipOutputFile, zipOutputFileErr := os.Create(codeZIPArchivePath)
	if zipOutputFileErr != nil {
		return errors.Wrapf(zipOutputFileErr, "Failed to create ZIP archive for code")
	}
	// Strip the local directory in case it's in there...
	relativeTempFilePath := relativePath(zipOutputFile.Name())
	lambdaArchive := zip.NewWriter(zipOutputFile)

	// Pass the state through the Metdata
	cpo.buildContext.cfTemplate.Metadata[MetadataParamCodeArchivePath] = relativeTempFilePath
	cpo.buildContext.cfTemplate.Metadata[MetadataParamServiceName] = cpo.userdata.serviceName
	cpo.buildContext.cfTemplate.Metadata[MetadataParamS3Bucket] = cpo.userdata.s3Bucket

	// Archive Hook
	archiveErr := callArchiveHook(lambdaArchive, cpo.userdata, cpo.buildContext, logger)
	if nil != archiveErr {
		return archiveErr
	}
	// Issue: https://github.com/mweagle/Sparta/issues/103. If the executable
	// bit isn't set, then AWS Lambda won't be able to fork the binary. This tends
	// to be set properly on a mac/linux os, but not on others. So pre-emptively
	// always set the bit.
	// Ref: https://github.com/mweagle/Sparta/issues/158
	fileHeaderAnnotator := func(header *zip.FileHeader) (*zip.FileHeader, error) {
		// Make the binary executable
		// Ref: https://github.com/aws/aws-lambda-go/blob/master/cmd/build-lambda-zip/main.go#L51
		header.CreatorVersion = 3 << 8
		header.ExternalAttrs = 0777 << 16
		return header, nil
	}

	// File info for the binary executable
	readerErr := spartaZip.AnnotateAddToZip(lambdaArchive,
		cpo.buildContext.compiledBinaryOutput,
		"",
		fileHeaderAnnotator,
		logger)
	if nil != readerErr {
		return readerErr
	}
	// Flush it...
	archiveCloseErr := lambdaArchive.Close()
	if nil != archiveCloseErr {
		return errors.Wrapf(archiveCloseErr, "Failed to close code ZIP stream")
	}
	zipCloseErr := zipOutputFile.Close()
	if zipCloseErr != nil {
		return errors.Wrapf(zipCloseErr, "Failed to close code ZIP archive")
	}
	logger.Info().
		Str("Path", zipOutputFile.Name()).
		Msg("Code Archive")
	return nil
}

type createTemplateOp struct {
	userdata     *userdata
	buildContext *buildContext
}

func (cto *createTemplateOp) Rollback(ctx context.Context, logger *zerolog.Logger) error {
	return nil
}
func (cto *createTemplateOp) insertTemplateParameters(ctx context.Context, logger *zerolog.Logger) (map[string]gocf.Stringable, error) {
	// Code archive...
	if cto.buildContext.cfTemplate.Parameters == nil {
		cto.buildContext.cfTemplate.Parameters = make(map[string]*gocf.Parameter)
	}
	paramRefMap := make(map[string]gocf.Stringable)

	// Code S3 info
	cto.buildContext.cfTemplate.Parameters[StackParamS3CodeKeyName] = newStackParameter(
		"String",
		"S3 key for object storing Sparta payload (required)",
		"",
		".+",
		3)
	paramRefMap[StackParamS3CodeKeyName] = gocf.Ref(StackParamS3CodeKeyName)
	cto.buildContext.cfTemplate.Parameters[StackParamS3CodeBucketName] = newStackParameter(
		"String",
		"S3 bucket for object storing Sparta payload (required)",
		"",
		".+",
		3)
	paramRefMap[StackParamS3CodeBucketName] = gocf.Ref(StackParamS3CodeBucketName)
	cto.buildContext.cfTemplate.Parameters[StackParamS3CodeVersion] = newStackParameter(
		"String",
		"S3 object version",
		"",
		".*",
		0)
	paramRefMap[StackParamS3CodeVersion] = gocf.Ref(StackParamS3CodeVersion)

	// Code Pipeline?
	if nil != codePipelineEnvironments {
		for _, eachEnvironment := range codePipelineEnvironments {
			for eachKey := range eachEnvironment {
				cto.buildContext.cfTemplate.Parameters[eachKey] = &gocf.Parameter{
					Type:    "String",
					Default: "",
				}
			}
		}
	}

	// Add the build time to the outputs...
	// Add the output
	cto.buildContext.cfTemplate.Outputs[StackOutputBuildTime] = &gocf.Output{
		Description: "UTC time template was created",
		Value:       gocf.String(time.Now().UTC().Format(time.RFC3339)),
	}
	cto.buildContext.cfTemplate.Outputs[StackOutputBuildID] = &gocf.Output{
		Description: "BuildID",
		Value:       gocf.String(cto.userdata.buildID),
	}
	return paramRefMap, nil
}

func (cto *createTemplateOp) ensureDiscoveryInfo(ctx context.Context, logger *zerolog.Logger) error {
	validateErrs := make([]error, 0)

	requiredEnvVars := []string{envVarDiscoveryInformation,
		envVarLogLevel}

	// Verify that all Lambda functions have discovery information
	for eachResourceID, eachResourceDef := range cto.buildContext.cfTemplate.Resources {
		switch typedResource := eachResourceDef.Properties.(type) {
		case *gocf.LambdaFunction:
			if typedResource.Environment == nil {
				validateErrs = append(validateErrs,
					errors.Errorf("Lambda function %s does not include environment info", eachResourceID))
			} else {
				vars, varsOk := typedResource.Environment.Variables.(map[string]interface{})
				if !varsOk {
					validateErrs = append(validateErrs,
						errors.Errorf("Lambda function %s environment vars are unsupported type: %T",
							eachResourceID,
							typedResource.Environment.Variables))
				} else {
					for _, eachKey := range requiredEnvVars {
						_, exists := vars[eachKey]
						if !exists {
							validateErrs = append(validateErrs,
								errors.Errorf("Lambda function %s environment does not include key: %s",
									eachResourceID,
									eachKey))
						}
					}
				}
			}
		}
	}
	if len(validateErrs) != 0 {
		return errors.Errorf("Problems validating template contents: %v", validateErrs)
	}
	return nil
}

func (cto *createTemplateOp) Invoke(ctx context.Context, logger *zerolog.Logger) error {

	// PreMarshall Hook
	if cto.userdata.workflowHooks != nil {
		preMarshallErr := callWorkflowHook("PreMarshall",
			cto.userdata.workflowHooks.PreMarshalls,
			cto.userdata,
			cto.buildContext,
			logger)
		if nil != preMarshallErr {
			return preMarshallErr
		}
	}

	//////////////////////////////////////////////////////////////////////////////
	// Add the "Parameters" to the template...
	paramMap, paramErrs := cto.insertTemplateParameters(ctx, logger)
	if paramErrs != nil {
		return paramErrs
	}

	//////////////////////////////////////////////////////////////////////////////
	// Add the tags
	stackTags := map[string]string{
		SpartaTagBuildIDKey: cto.userdata.buildID,
	}
	if len(cto.userdata.buildTags) != 0 {
		stackTags[SpartaTagBuildTagsKey] = cto.userdata.buildTags
	}

	// Canonical code resource definition...
	s3CodeResource := &gocf.LambdaFunctionCode{
		S3Bucket:        paramMap[StackParamS3CodeBucketName].String(),
		S3Key:           paramMap[StackParamS3CodeKeyName].String(),
		S3ObjectVersion: paramMap[StackParamS3CodeVersion].String(),
	}

	// Marshall the objects...
	for _, eachEntry := range cto.userdata.lambdaAWSInfos {
		verifyErr := verifyLambdaPreconditions(eachEntry, logger)
		if verifyErr != nil {
			return verifyErr
		}
		annotateCodePipelineEnvironments(eachEntry, logger)

		exportContext, exportErr := eachEntry.export(cto.buildContext.workflowHooksContext,
			cto.userdata.serviceName,
			s3CodeResource,
			cto.userdata.buildID,
			cto.buildContext.lambdaIAMRoleNameMap,
			cto.buildContext.cfTemplate,
			logger)
		if nil != exportErr {
			return exportErr
		}
		cto.buildContext.workflowHooksContext = exportContext
	}
	// If there's an API gateway definition, include the resources that provision it. Since this export will likely
	// generate outputs that the s3 site needs, we'll use a temporary outputs accumulator, pass that to the S3Site
	// if it's defined, and then merge it with the normal output map.
	apiGatewayTemplate := gocf.NewTemplate()

	if nil != cto.userdata.api {
		err := cto.userdata.api.Marshal(
			cto.userdata.serviceName,
			cto.buildContext.awsSession,
			s3CodeResource,
			cto.buildContext.lambdaIAMRoleNameMap,
			apiGatewayTemplate,
			cto.userdata.noop,
			logger)
		if nil == err {
			safeMergeErrs := gocc.SafeMerge(apiGatewayTemplate,
				cto.buildContext.cfTemplate)
			if len(safeMergeErrs) != 0 {
				err = errors.Errorf("APIGateway template merge failed: %v", safeMergeErrs)
			}
		}
		if nil != err {
			return errors.Wrapf(err, "APIGateway template export failed")
		}
	}
	// Service decorator?
	// This is run before the S3 Site in case the decorators
	// need to publish data to the MANIFEST for the site
	serviceDecoratorErr := callServiceDecoratorHook(s3CodeResource,
		cto.userdata,
		cto.buildContext,
		logger)
	if serviceDecoratorErr != nil {
		return serviceDecoratorErr
	}

	// Discovery info on a per-function basis
	for _, eachEntry := range cto.userdata.lambdaAWSInfos {
		_, annotateErr := annotateDiscoveryInfo(eachEntry, cto.buildContext.cfTemplate, logger)
		if annotateErr != nil {
			return annotateErr
		}
		_, annotateErr = annotateBuildInformation(eachEntry,
			cto.buildContext.cfTemplate,
			cto.userdata.buildID,
			logger)
		if annotateErr != nil {
			return annotateErr
		}
		// Any custom resources? These may also need discovery info
		// so that they can self-discover the stack name
		for _, eachCustomResource := range eachEntry.customResources {
			discoveryInfo, discoveryInfoErr := discoveryInfoForResource(eachCustomResource.logicalName(),
				nil)
			if discoveryInfoErr != nil {
				return discoveryInfoErr
			}
			logger.Info().
				Interface("Discovery", discoveryInfo).
				Str("Resource", eachCustomResource.logicalName()).
				Msg("Annotating discovery info for custom resource")

			// Update the env map
			eachCustomResource.options.Environment[envVarDiscoveryInformation] = discoveryInfo
		}
	}
	// There is no way to get this unless it's a param ref...

	// If there's a Site defined, include the resources the provision it
	// TODO - turn this into a Parameter block with defaults...
	if nil != cto.userdata.s3SiteContext.s3Site {
		exportErr := cto.userdata.s3SiteContext.s3Site.export(cto.userdata.serviceName,
			SpartaBinaryName,
			gocf.Ref(StackParamS3CodeBucketName),
			s3CodeResource,
			gocf.Ref(StackParamS3SiteArchiveKey).String(),
			apiGatewayTemplate.Outputs,
			cto.buildContext.lambdaIAMRoleNameMap,
			cto.buildContext.cfTemplate,
			logger)
		if exportErr != nil {
			return errors.Wrapf(exportErr, "Failed to export S3 site")
		}
	}

	// PostMarshall Hook
	if cto.userdata.workflowHooks != nil {
		postMarshallErr := callWorkflowHook("PostMarshall",
			cto.userdata.workflowHooks.PostMarshalls,
			cto.userdata,
			cto.buildContext,
			logger)
		if nil != postMarshallErr {
			return postMarshallErr
		}
	}

	// Last step, run the annotation steps to patch
	// up any references that depends on the entire
	// template being constructed
	_, annotateErr := annotateMaterializedTemplate(cto.userdata.lambdaAWSInfos,
		cto.buildContext.cfTemplate,
		logger)
	if annotateErr != nil {
		return errors.Wrapf(annotateErr,
			"Failed to perform final template annotations")
	}

	// validations?
	if cto.userdata.workflowHooks != nil {
		validationErr := callValidationHooks(cto.userdata.workflowHooks.Validators,
			cto.buildContext.cfTemplate,
			s3CodeResource,
			cto.userdata,
			cto.buildContext,
			logger)
		if validationErr != nil {
			return validationErr
		}
	}

	// Ensure there is discovery info in the template
	discoveryInfoErr := cto.ensureDiscoveryInfo(ctx, logger)
	if nil != discoveryInfoErr {
		return discoveryInfoErr
	}

	// Generate it & write it out...
	cfTemplateJSON, cfTemplateJSONErr := json.Marshal(cto.buildContext.cfTemplate)
	if cfTemplateJSONErr != nil {
		logger.Error().
			Err(cfTemplateJSONErr).
			Msg("Failed to Marshal CloudFormation template")
		return cfTemplateJSONErr
	}

	// Write out the template to the templateWriter
	if nil != cto.buildContext.templateWriter {
		_, writeErr := cto.buildContext.templateWriter.Write(cfTemplateJSON)
		if nil != writeErr {
			return writeErr
		}
	}
	if logger.GetLevel() <= zerolog.DebugLevel {
		formatted, formattedErr := json.Marshal(string(cfTemplateJSON))
		if formattedErr == nil {
			logger.Debug().
				Str("Body", string(formatted)).
				Msg("CloudFormation template body")
		}
	}
	return nil
}

// Build is just build the artifacts - update template to take params
// If we decouple build and deploy then there's a spot for user to plug in UPX
// Deploy uploads the artifacts. So for that we'll need what, the S3 bucket?

// Build compiles a Sparta application and produces the single binary
// that represents the entire service
// The serviceName is the service's logical
// identify and is used to determine create vs update operations.  The compilation options/flags are:
//
// 	TAGS:         -tags lambdabinary
// 	ENVIRONMENT:  GOOS=linux GOARCH=amd64
//
// The compiled binary is packaged with a NodeJS proxy shim to manage AWS Lambda setup & invocation per
// http://docs.aws.amazon.com/lambda/latest/dg/authoring-function-in-nodejs.html
//
// The two files are ZIP'd, posted to S3 and used as an input to a dynamically generated CloudFormation
// template (http://docs.aws.amazon.com/AWSCloudFormation/latest/UserGuide/Welcome.html)
// which creates or updates the service state.
//
func Build(noop bool,
	serviceName string,
	serviceDescription string,
	lambdaAWSInfos []*LambdaAWSInfo,
	api APIGateway,
	site *S3Site,
	useCGO bool,
	buildID string,
	outputDirectory string,
	buildTags string,
	linkerFlags string,
	templateWriter io.Writer,
	workflowHooks *WorkflowHooks,
	logger *zerolog.Logger) error {

	// Mutable data
	userdata := &userdata{
		noop:               noop,
		useCGO:             useCGO,
		buildID:            buildID,
		buildTags:          buildTags,
		linkFlags:          linkerFlags,
		serviceName:        serviceName,
		serviceDescription: serviceDescription,
		lambdaAWSInfos:     lambdaAWSInfos,
		api:                api,
		s3SiteContext: &s3SiteContext{
			s3Site: site,
		},
		workflowHooks: workflowHooks,
	}

	absOutputDirectory, absOutputDirectoryErr := filepath.Abs(outputDirectory)
	if absOutputDirectoryErr != nil {
		return absOutputDirectoryErr
	}
	buildContext := &buildContext{
		cfTemplate:           gocf.NewTemplate(),
		awsSession:           spartaAWS.NewSession(logger),
		outputDirectory:      absOutputDirectory,
		workflowHooksContext: nil,
		templateWriter:       templateWriter,
		compiledBinaryOutput: filepath.Join(absOutputDirectory, SpartaBinaryName),
	}
	if workflowHooks != nil && workflowHooks.Context != nil {
		buildContext.workflowHooksContext = workflowHooks.Context
	} else {
		buildContext.workflowHooksContext = context.Background()
	}
	buildContext.cfTemplate.Description = serviceDescription

	// Add some params to the context...
	buildContext.workflowHooksContext = context.WithValue(buildContext.workflowHooksContext,
		ContextKeyBuildOutputDir,
		outputDirectory)
	buildContext.workflowHooksContext = context.WithValue(buildContext.workflowHooksContext,
		ContextKeyBuildID,
		buildID)
	buildContext.workflowHooksContext = context.WithValue(buildContext.workflowHooksContext,
		ContextKeyBuildBinaryName,
		SpartaBinaryName)

	logger.Info().
		Str("BuildID", buildID).
		Bool("noop", noop).
		Str("Tags", userdata.buildTags).
		Str("CodePipelineTrigger", userdata.codePipelineTrigger).
		Bool("InPlaceUpdates", userdata.inPlace).
		Msg("Building service")

	//////////////////////////////////////////////////////////////////////////////
	// Workflow
	//////////////////////////////////////////////////////////////////////////////
	/* #nosec G104 */

	var rollbackFuncs []RollbackHookHandler
	if workflowHooks != nil {
		rollbackFuncs = workflowHooks.Rollbacks
	}
	buildPipeline := newUserRollbackEnabledPipeline(
		serviceName,
		buildContext.awsSession,
		rollbackFuncs,
		noop)

	// Verify
	stageAWSPreconditions := &pipelineStage{}
	stageAWSPreconditions.Append("validateAWSPreconditions", &verifyAWSPreconditionsOp{
		userdata: userdata,
	})
	buildPipeline.Append("validateAWSPreconditions", stageAWSPreconditions)

	stageVerify := &pipelineStage{}
	stageVerify.Append("validatePreconditions", &validatePreconditionsOp{
		userdata: userdata,
	})
	stageVerify.Append("validateIAMRoles", &verifyIAMRolesOp{
		userdata:     userdata,
		buildContext: buildContext,
	})
	buildPipeline.Append("validate", stageVerify)

	// Build Package
	stageIAM := &pipelineStage{}
	stageIAM.Append("verifyIAM", &createPackageOp{
		userdata:     userdata,
		buildContext: buildContext,
	})
	buildPipeline.Append("verifyIAM", stageIAM)

	// Create the CloudFormation template
	// Build Package
	stageCreateTemplate := &pipelineStage{}
	stageCreateTemplate.Append("createTemplate", &createTemplateOp{
		userdata:     userdata,
		buildContext: buildContext,
	})
	buildPipeline.Append("createTemplate", stageCreateTemplate)

	pipelineContext := context.Background()
	buildErr := buildPipeline.Run(pipelineContext, "Build", logger)
	if buildErr != nil {
		return buildErr
	}
	return nil
}
