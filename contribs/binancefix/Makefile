GO ?= $(shell which go)

GO_TOOLS_GOLANGCI_LINT_VERSION ?= v2.1.6
GO_TOOLS_GOLANGCI_LINT         ?= $(shell $(GO) env GOPATH)/bin/golangci-lint-$(GO_TOOLS_GOLANGCI_LINT_VERSION)

# binancefix is a library: nothing to build but its tests.
.DEFAULT_GOAL := test

# -- go generate ---------------------------------------------------------------

.PHONY: generate generate-verify

# Regenerates internal/fixsbe from the vendored schema with fix2gen, and
# codecs.go, the aliases exporting it.
generate:
	$(GO) generate ./...

generate-verify: generate
	git diff --exit-code -- internal/fixsbe codecs.go

# -- go mod --------------------------------------------------------------------

.PHONY: go-mod-verify go-mod-tidy

go-mod-verify:
	$(GO) mod download
	git diff --quiet go.* || git diff --exit-code go.* || exit 1

go-mod-tidy:
	$(GO) mod download
	$(GO) mod tidy

# ------------------------------------------------------------------------------

.PHONY: test test-race lint

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

lint: $(GO_TOOLS_GOLANGCI_LINT)
	$(GO_TOOLS_GOLANGCI_LINT) run

# -- tools ---------------------------------------------------------------------

.PHONY: tools

tools: $(GO_TOOLS_GOLANGCI_LINT)

$(GO_TOOLS_GOLANGCI_LINT):
	tmp=$$(mktemp -d) && \
		GOBIN=$$tmp $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GO_TOOLS_GOLANGCI_LINT_VERSION) && \
		mkdir -p $(dir $@) && mv $$tmp/golangci-lint $@ && rmdir $$tmp
