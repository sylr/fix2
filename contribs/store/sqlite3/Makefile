GO ?= $(shell which go)

GO_TOOLS_GOLANGCI_LINT_VERSION ?= v2.1.6
GO_TOOLS_GOLANGCI_LINT         ?= $(shell $(GO) env GOPATH)/bin/golangci-lint-$(GO_TOOLS_GOLANGCI_LINT_VERSION)

# go-sqlite3 is a cgo package: everything here needs a C compiler.
export CGO_ENABLED := 1

# -- go mod --------------------------------------------------------------------

.PHONY: go-mod-verify go-mod-tidy

go-mod-verify:
	$(GO) mod download
	git diff --quiet go.* || git diff --exit-code go.* || exit 1

go-mod-tidy:
	$(GO) mod download
	$(GO) mod tidy

# ------------------------------------------------------------------------------

.PHONY: test test-race bench lint

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

bench:
	$(GO) test -run '^$$' -bench . ./...

lint: $(GO_TOOLS_GOLANGCI_LINT)
	$(GO_TOOLS_GOLANGCI_LINT) run

# -- tools ---------------------------------------------------------------------

.PHONY: tools

tools: $(GO_TOOLS_GOLANGCI_LINT)

$(GO_TOOLS_GOLANGCI_LINT):
	tmp=$$(mktemp -d) && \
		GOBIN=$$tmp $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GO_TOOLS_GOLANGCI_LINT_VERSION) && \
		mkdir -p $(dir $@) && mv $$tmp/golangci-lint $@ && rmdir $$tmp
