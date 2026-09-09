SHELL := /usr/bin/env bash

GO ?= go
CONTROLLER_GEN ?= $(GO) tool controller-gen
SETUP_ENVTEST ?= $(shell $(GO) env GOPATH)/bin/setup-envtest
IMG ?= ghcr.io/opentelekomcloud/t-cloud-public-rancher-network-controller:dev

.PHONY: all fmt vet test test-race test-integration build manifests generate docker-build

all: fmt vet test build

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

test:
	$(GO) test ./... -coverprofile cover.out

test-race:
	$(GO) test -race ./...

test-integration:
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use 1.34.x -p path)" $(GO) test -tags=integration ./internal/...

build:
	$(GO) build -o bin/manager ./cmd

generate:
	$(CONTROLLER_GEN) object paths="./api/..."

manifests:
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd paths="./..." output:crd:artifacts:config=config/crd/bases
	$(CONTROLLER_GEN) crd paths="./api/..." output:crd:artifacts:config=charts/t-cloud-network-controller/crds

docker-build:
	docker build -t $(IMG) .
