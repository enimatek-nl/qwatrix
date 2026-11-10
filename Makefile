# qwatrix — build, test and compliance targets. `make` prints this list.
#
# Every setting is overridable on the command line, e.g.
#   make run ADDR=127.0.0.1:9000
#   make complement COMPLEMENT_RUN='TestSync|TestRoomState'

GO       ?= go
BIN_DIR  ?= bin
BINARY   ?= qwatrix

ADDR                   ?= 127.0.0.1:8008
SERVER_NAME            ?= localhost:8008
DATA_DIR               ?= /tmp/qwatrix

# The Complement checkout that owns the upstream suite. Complement is not a module
# dependency of that suite (its own go.mod owns those test binaries); our own out-of-repo
# tests in tests/complement *are* a dependency and live in this module.
COMPLEMENT       ?= $(HOME)/complement
COMPLEMENT_PKG   ?= ./tests/csapi
COMPLEMENT_RUN   ?=
COMPLEMENT_IMAGE ?= localhost/qwatrix-complement:latest
# Podman speaks the Docker API but needs the Docker image format (OCI has no HEALTHCHECK
# directive, which Complement waits on) and uses another hostname for the host. BUILDAH_FORMAT
# has to be exported, not just set: it is read by the builder process, not by make.
export BUILDAH_FORMAT ?= docker
COMPLEMENT_HOST       ?= host.containers.internal
COMPLEMENT_ENV        ?= COMPLEMENT_HOSTNAME_RUNNING_COMPLEMENT=$(COMPLEMENT_HOST)

IMAGE ?= qwatrix:latest

.DEFAULT_GOAL := help

.PHONY: help build image test race vet fmt lint run clean complement-image complement complement-smoke

help: ## List the available targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  %-19s %s\n", $$1, $$2}'

build: ## Build the homeserver into bin/qwatrix
	CGO_ENABLED=0 $(GO) build -trimpath -o $(BIN_DIR)/$(BINARY) ./cmd/qwatrix

test: ## Run the in-process suites (unit + API compatibility)
	$(GO) test ./tests/...

race: ## Run the in-process suites under the race detector
	$(GO) test -race -count=1 ./tests/...

vet: ## go vet across every package
	$(GO) vet ./...

fmt: ## gofmt -s across every Go file
	gofmt -s -w cmd internal tests

lint: ## golangci-lint across every package
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint is not installed: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest"; \
		exit 1; \
	}
	golangci-lint run ./...

run: build ## Run a development server on loopback (see the variables above)
	mkdir -p $(DATA_DIR)
	$(BIN_DIR)/$(BINARY) -addr $(ADDR) -server-name $(SERVER_NAME) -data-dir $(DATA_DIR)

# The image Complement tests: a binary built outside the image plus an entrypoint that turns
# the authority Complement mounts at /complement/ca into a certificate named for the server.
# CGO is off because the runtime base is musl — see the Dockerfile.
complement-image: ## Build the Complement container image for this binary
	CGO_ENABLED=0 GOOS=linux $(GO) build -trimpath -ldflags '-s -w' -o $(BIN_DIR)/$(BINARY) ./cmd/qwatrix
	docker build -t $(COMPLEMENT_IMAGE) -f deploy/complement/Dockerfile .

# `make complement-smoke` answers the only question that matters before writing more
# features: can Complement stand our image up at all? It runs the out-of-repo package in
# tests/complement, which asserts the container contract (ports, readiness, shared-secret
# registration, two-server deployment).
complement-smoke: complement-image ## Run the Complement container-contract tests
	COMPLEMENT_BASE_IMAGE=$(COMPLEMENT_IMAGE) $(COMPLEMENT_ENV) \
		$(GO) test -tags complement -count=1 -timeout=15m -v ./tests/complement

# `make complement` runs Complement's own suite from its checkout against this image.
# Narrow it with COMPLEMENT_RUN, e.g.
#   make complement COMPLEMENT_RUN='TestRoomCreate|TestRoomState'
# and point it at the server-server suite with COMPLEMENT_PKG=./tests.
complement: complement-image ## Run Complement's upstream suite against the image
	cd $(COMPLEMENT) && $(COMPLEMENT_ENV) COMPLEMENT_BASE_IMAGE=$(COMPLEMENT_IMAGE) \
		$(GO) test -timeout=60m $(COMPLEMENT_PKG) \
		$(if $(COMPLEMENT_RUN),-run '$(COMPLEMENT_RUN)',)

image: ## Build the release container image
	docker build -t $(IMAGE) -f deploy/Dockerfile .

clean: ## Remove build output
	rm -rf $(BIN_DIR)
