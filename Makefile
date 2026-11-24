SHELL := /bin/bash
# Bison9 Agent SDK — build and test entry points
# (see ~/.claude/standards/makefile/makefile.md).
#
#   make              run the full test suite, then build (only if the tests pass)
#   make test         the one gate (./test.sh): format check, go vet, vulnerability
#                     scan, golangci-lint and go test -race, with a summary; exits
#                     non-zero on any failure
#   make build        compile the packages (a library: nothing is produced)
#   make clean        drop the Go test cache (the suite leaves no artifacts in the tree)
#   make fmt          rewrite formatting (the gate only verifies it)
#   make vet          go vet on its own
#   make lint         golangci-lint on its own
#   make govulncheck  the vulnerability scan on its own
#   make update       update every dependency to its latest version
#
# There is no install target: this is a library, consumed as a Go module.

# golangci-lint: on PATH if present, otherwise the Go bin directory (go install
# puts it there, which is not on everyone's PATH). Override with GOLANGCI_LINT=.
GOLANGCI_LINT ?= $(shell command -v golangci-lint 2>/dev/null || echo "$$(go env GOPATH)/bin/golangci-lint")
export GOLANGCI_LINT

.PHONY: all test build clean fmt vet lint govulncheck update require-golangci-lint

all: test build

# The gate runs every stage, the vulnerability scan (govulncheck, below)
# included, and reports them together.
test:
	@./test.sh

# A library has no binaries; this only proves every package compiles.
build:
	@go build ./...

clean:
	@go clean -testcache

fmt: require-golangci-lint
	@$(GOLANGCI_LINT) fmt

vet:
	@go vet ./...

lint: require-golangci-lint
	@$(GOLANGCI_LINT) run ./...

govulncheck:
	@go run go.bison9.com/vulncheck/cmd/vulncheck@latest .

update:
	go get -u ./...
	go mod tidy

# Fail with the install command rather than skipping: the gate is not complete
# without the linter, and global tools are never installed on the user's behalf.
require-golangci-lint:
	@test -x "$(GOLANGCI_LINT)" || { \
	  echo "ERROR: golangci-lint not found (looked for $(GOLANGCI_LINT))."; \
	  echo "Install it with: go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest"; \
	  echo "or set GOLANGCI_LINT=/path/to/golangci-lint"; exit 1; }
