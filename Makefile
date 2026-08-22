PLUGIN_ID := model-router
VERSION ?= 0.4.1
GOOS := $(shell go env GOOS)

ifeq ($(GOOS),windows)
LIB_EXT := dll
else ifeq ($(GOOS),darwin)
LIB_EXT := dylib
else
LIB_EXT := so
endif

.PHONY: fmt fmt-check test race vet check build clean

fmt:
	gofmt -w *.go

fmt-check:
	@test -z "$$(gofmt -l *.go)" || { gofmt -l *.go; exit 1; }

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

check: fmt-check test race vet

build:
	mkdir -p dist
	CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -ldflags="-s -w -X main.pluginVersion=$(VERSION)" -o dist/$(PLUGIN_ID).$(LIB_EXT) .

clean:
	go clean
