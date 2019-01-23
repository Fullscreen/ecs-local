VERSION  := $(shell cat ./VERSION)
GIT_HASH := $(shell git rev-parse HEAD)

XC_OS ?= darwin linux
XC_ARCH ?= 386 amd64
LDFLAGS :=-X main.Version=$(VERSION)

all: build

deps:
	@go get -t ./...

clean:
	@rm -rf dist/*

build:
	@gox -os="$(XC_OS)" -arch="$(XC_ARCH)" -ldflags "$(LDFLAGS)" -output "dist/{{.OS}}_{{.Arch}}/{{.Dir}}"

release:
	git tag -a $(VERSION) -m "Release" || true
	git push origin $(VERSION)
	goreleaser --rm-dist

.PHONY: clean release
