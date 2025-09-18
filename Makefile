SHELL := bash
.SHELLFLAGS := -euo pipefail -c
.DEFAULT_GOAL := all
.DELETE_ON_ERROR:
MAKEFLAGS += --no-builtin-rules
MAKEFLAGS += --jobs
MAKEFLAGS += --shuffle=random
MAKEFLAGS += --warn-undefined-variables

BINARY_OUT ?= ./lmk
CMD_DIR := ./
CGO_ENABLED ?= 0

TEST_COVERAGE_OUT := ./.gocoverage

.PHONY: all
all:
	$(MAKE) lint
	$(MAKE) test
	$(MAKE) build

.PHONY: lint
lint:
	@golangci-lint run -v \
		./...

.PHONY: test-fast
test-fast:
	CGO_ENABLED="${CGO_ENABLED}" go test -v \
		-shuffle on \
		-failfast \
		./...

.PHONY: test
test: test-fast
	CGO_ENABLED="1" go test -v \
		-shuffle on \
		-vet=all \
		-race \
		-cover -covermode=atomic -coverprofile="${TEST_COVERAGE_OUT}" \
		./...

.PHONY: test-cover-open
test-cover-open: test
	go tool cover \
		-html="${TEST_COVERAGE_OUT}"

BUILD_FLAGS ?=
.PHONY: build
build:
	CGO_ENABLED="${CGO_ENABLED}" go build -v \
		-o "${BINARY_OUT}" \
		${BUILD_FLAGS} \
		"${CMD_DIR}"

DEBUG ?= true
.PHONY: run
run:
	DEBUG="${DEBUG}" \
	CGO_ENABLED="${CGO_ENABLED}" \
		air -c ./.air.toml

.PHONY: clean
clean:
	go clean -r -cache -testcache -modcache -fuzzcache
