.PHONY: test deps

PKGS=`go list ./... | grep -v /vendor/`
LOCALS=`find . -type f -name '*.go' -not -path "./vendor/*"`

all: deps fmt test build

deps-glide:
	@glide install --strip-vendor

deps:
	dep ensure

clean-bundle:
	-rm -rf public

clean:
	-rm -rf bin

fmt:
	@go list github.com/mjibson/esc || go get github.com/mjibson/esc/...
	@go list golang.org/x/tools/cmd/goimports || go get golang.org/x/tools/cmd/goimports
	go generate -x ./...
	goimports -w $(LOCALS)
	go vet $(PKGS)

test:
	go test --tags json1 $(PKGS)

integration:
	INTEGRATION=1 go test --tags json1 $(PKGS)

build:
	test -d pivot && go build --tags json1 -i -o bin/`basename ${PWD}` pivot/*.go

quickbuild: deps-glide fmt
	test -d pivot && go build -i -o bin/`basename ${PWD}` pivot/*.go
