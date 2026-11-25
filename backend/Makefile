GOFUMPT := $(shell command -v gofumpt 2>/dev/null || echo $(HOME)/go/bin/gofumpt)
GOLANGCI := $(shell command -v golangci-lint 2>/dev/null || echo $(HOME)/go/bin/golangci-lint)

.PHONY: fmt lint vet test check

fmt:
	$(GOFUMPT) -w internal/ cmd/

lint:
	$(GOLANGCI) run ./...

vet:
	go vet ./...

test:
	go test ./...

check: fmt vet lint test
