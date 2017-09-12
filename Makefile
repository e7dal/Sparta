.DEFAULT_GOAL=build
.PHONY: build test run tags clean reset

clean:
	go clean .
	go env

reset:
		git reset --hard
		git clean -f -d

generate:
	go generate -x . ./proto
	./stamp.sh
	./embed.sh
	@echo "Generate complete: `date`"

validate:
	goimports -d *.go
	goimports -d ./explore
	goimports -d ./aws/
	goimports -d ./docker/

format:
	go fmt .

build: format generate validate
	go build .
	@echo "Build complete"

docs:
	@echo ""
	@echo "Sparta godocs: http://localhost:8090/pkg/github.com/mweagle/Sparta"
	@echo
	godoc -v -http=:8090 -index=true

travis-depends:
	go get -u github.com/golang/dep/...
	dep ensure
	go get -u golang.org/x/tools/cmd/goimports
	rm -rf $(GOPATH)/src/github.com/mjibson/esc
	git clone --depth=1 https://github.com/mjibson/esc $(GOPATH)/src/github.com/mjibson/esc
	rm -rf $(GOPATH)/src/github.com/fzipp/gocyclo
	git clone --depth=1 https://github.com/fzipp/gocyclo $(GOPATH)/src/github.com/fzipp/gocyclo
	# Move everything in the ./vendor directory to the $(GOPATH)/src directory
	rsync -a --quiet --remove-source-files ./vendor/ $(GOPATH)/src

travis-ci-test: travis-depends build
	go test -v .
	go test -v ./aws/...

test: build
	go test -v .
	go test -v ./aws/...

run: build
	./sparta

tags:
	gotags -tag-relative=true -R=true -sort=true -f="tags" -fields=+l .

provision: build
	go run ./applications/hello_world.go --level info provision --s3Bucket $(S3_BUCKET)

execute: build
	./sparta execute

describe: build
	rm -rf ./graph.html
	go test -v -run TestDescribe

publish: build
	git commit -a -m "Publishing latest autogenerated updates"
	git push origin