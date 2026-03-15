.PHONY: all build clean test install

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
GIT_COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo "unknown")

# Build output directory
BUILD_DIR := build

all: build

build: build-agent build-cni

build-agent:
	@echo "Building nebula-nomad-agent..."
	@mkdir -p $(BUILD_DIR)
	go build  -o $(BUILD_DIR)/nebula-nomad-agent ./cmd/nebula-nomad-agent

build-cni:
	@echo "Building nebula-nomad-cni..."
	@mkdir -p $(BUILD_DIR)
	go build  -o $(BUILD_DIR)/nebula-nomad-cni ./cmd/nebula-nomad-cni

clean:
	@echo "Cleaning build artifacts..."
	rm -rf $(BUILD_DIR)

test:
	@echo "Running tests..."
	go test -v ./...

fmt:
	@echo "Formatting code..."
	go fmt ./...

vet:
	@echo "Vetting code..."
	go vet ./...

lint: fmt vet
	@echo "Linting complete"
