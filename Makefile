APP        ?= libsql-bkup
IMAGE      ?= jkaninda/libsql-bkup
VERSION    ?= dev
LDFLAGS    := -s -w -X 'main.Version=$(VERSION)'
PLATFORMS  ?= linux/amd64,linux/arm64,linux/arm/v7


BUILDER ?= libsql-bkup-builder


HOST_ARCH := $(shell uname -m | sed -e 's/x86_64/amd64/' -e 's/aarch64/arm64/')

.PHONY: help build compile test test-integration vet tidy fmt lint clean env-docs \
        buildx-setup buildx-clean docker docker-build docker-buildx docker-push release

## help: list the available targets
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //'

## build: build for the host
build:
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o bin/$(APP) .

## compile: build release binaries for every supported OS/arch
compile:
	GOOS=linux  GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o bin/$(APP)-linux-amd64 .
	GOOS=linux  GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o bin/$(APP)-linux-arm64 .
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o bin/$(APP)-darwin-amd64 .
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o bin/$(APP)-darwin-arm64 .

## test: run the unit tests
test:
	go test -race ./...

## test-integration: run the integration tests (needs docker: sqld, MinIO)
test-integration:
	go test -tags integration -count=1 -timeout 20m ./...

## vet: run go vet (unit and integration build tags)
vet:
	go vet ./...
	go vet -tags integration ./...

## lint: run golangci-lint
lint:
	golangci-lint run --build-tags integration

## tidy: tidy go.mod (outside the go.work workspace)
tidy:
	GOFLAGS=-mod=mod GOWORK=off go mod tidy

## env-docs: print the environment variable reference (README)
env-docs:
	@go run . config --env-docs

## fmt: format the source
fmt:
	gofmt -w .

## clean: remove build output
clean:
	rm -rf bin

## buildx-setup: create the multi-arch builder (idempotent)
buildx-setup:
	@docker buildx inspect $(BUILDER) >/dev/null 2>&1 || \
		docker buildx create --name $(BUILDER) --driver docker-container --bootstrap >/dev/null
	@docker buildx inspect $(BUILDER) --bootstrap >/dev/null

## docker: build for the HOST architecture and load it into Docker, so it can be
##         run locally. A multi-arch build cannot be loaded — the local image
##         store holds no manifest lists — hence the separate targets below.
docker: buildx-setup
	docker buildx build \
		--builder $(BUILDER) \
		--platform linux/$(HOST_ARCH) \
		--build-arg appVersion=$(VERSION) \
		--tag $(IMAGE):$(VERSION) \
		--load .

## docker-build: alias of docker
docker-build: docker

## docker-buildx: build linux/amd64 + linux/arm64 without publishing, to prove
##                both architectures compile. Pushing is docker-push, never a
##                side effect of checking that it builds.
docker-buildx: buildx-setup
	docker buildx build \
		--builder $(BUILDER) \
		--platform $(PLATFORMS) \
		--build-arg appVersion=$(VERSION) \
		--tag $(IMAGE):$(VERSION) .

## docker-push: build linux/amd64 + linux/arm64 and push the manifest list
##              (needs `docker login`)
docker-push: buildx-setup
	docker buildx build \
		--builder $(BUILDER) \
		--platform $(PLATFORMS) \
		--build-arg appVersion=$(VERSION) \
		--tag $(IMAGE):$(VERSION) \
		--push .


release: buildx-setup
	@test "$(VERSION)" != "dev" || { echo "release needs a version: make release VERSION=1.2.3"; exit 1; }
	docker buildx build \
		--builder $(BUILDER) \
		--platform $(PLATFORMS) \
		--build-arg appVersion=$(VERSION) \
		--tag $(IMAGE):$(VERSION) \
		--tag $(IMAGE):latest \
		--push .

## buildx-clean: remove the builder and its cache
buildx-clean:
	-docker buildx rm $(BUILDER)
