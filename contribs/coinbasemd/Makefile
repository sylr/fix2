GO                ?= $(shell which go)
GIT_UPDATE_INDEX  := $(shell git update-index --refresh > /dev/null 2>&1)
GIT_REVISION      ?= $(shell git rev-parse HEAD)
GIT_VERSION       ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

GOENV_GOOS               := $(shell $(GO) env GOOS)
GOENV_GOARCH             := $(shell $(GO) env GOARCH)
GOENV_GOARM              := $(shell $(GO) env GOARM)
CGO_ENABLED              ?= 0
GOOS                     ?= $(GOENV_GOOS)
GOARCH                   ?= $(GOENV_GOARCH)
GOARM                    ?= $(GOENV_GOARM)
# The binary builds against the fix2 of the repository (replace directive):
# its sources count too.
GO_BUILD_SRC             := $(shell find . -name \*.go -type f) go.mod go.sum internal/fixt/FIXT11-prod-sand.xml internal/md/FIX50-prod-sand.xml
GO_BUILD_SRC             += $(shell find ../.. -path ../../contribs -prune -o -name \*.go -type f -print) ../../go.mod ../../go.sum
GO_BUILD_EXTLDFLAGS      ?=
GO_BUILD_TAGS            ?=
GO_BUILD_TARGET_DEPS     :=
GO_BUILD_FLAGS           := -trimpath
GO_BUILD_LDFLAGS_OPTIMS  ?= -s -w

ifeq ($(GOOS)/$(GOARCH),$(GOENV_GOOS)/$(GOENV_GOARCH))
GO_BUILD_TARGET          ?= dist/coinbasemd
GO_BUILD_VERSION_TARGET  ?= dist/coinbasemd-$(GIT_VERSION)
else
ifeq ($(GOARCH),arm)
GO_BUILD_TARGET          ?= dist/coinbasemd-$(GOOS)-$(GOARCH)v$(GOARM)
GO_BUILD_VERSION_TARGET  := dist/coinbasemd-$(GIT_VERSION)-$(GOOS)-$(GOARCH)v$(GOARM)
else
GO_BUILD_TARGET          ?= dist/coinbasemd-$(GOOS)-$(GOARCH)
GO_BUILD_VERSION_TARGET  := dist/coinbasemd-$(GIT_VERSION)-$(GOOS)-$(GOARCH)
endif # ($(GOARCH),arm)
endif # ($(GOOS)/$(GOARCH),$(GOENV_GOOS)/$(GOENV_GOARCH))

ifneq ($(DEBUG),)
GO_BUILD_FLAGS            = -gcflags="all=-N -l"
GO_BUILD_LDFLAGS_OPTIMS   =
endif # $(DEBUG)

GO_BUILD_FLAGS_TARGET   := .go-build-flags
GO_CROSSBUILD_LINUX_AMD64_TARGET   := dist/coinbasemd-$(GIT_VERSION)-linux-amd64
GO_CROSSBUILD_LINUX_ARM64_TARGET   := dist/coinbasemd-$(GIT_VERSION)-linux-arm64
GO_CROSSBUILD_DARWIN_AMD64_TARGET  := dist/coinbasemd-$(GIT_VERSION)-darwin-amd64
GO_CROSSBUILD_DARWIN_ARM64_TARGET  := dist/coinbasemd-$(GIT_VERSION)-darwin-arm64
GO_CROSSBUILD_WINDOWS_AMD64_TARGET := dist/coinbasemd-$(GIT_VERSION)-windows-amd64.exe
GO_CROSSBUILD_WINDOWS_ARM64_TARGET := dist/coinbasemd-$(GIT_VERSION)-windows-arm64.exe

GO_CROSSBUILD_TARGETS  = $(GO_CROSSBUILD_LINUX_AMD64_TARGET)
GO_CROSSBUILD_TARGETS += $(GO_CROSSBUILD_LINUX_ARM64_TARGET)
GO_CROSSBUILD_TARGETS += $(GO_CROSSBUILD_DARWIN_AMD64_TARGET)
GO_CROSSBUILD_TARGETS += $(GO_CROSSBUILD_DARWIN_ARM64_TARGET)
GO_CROSSBUILD_TARGETS += $(GO_CROSSBUILD_WINDOWS_AMD64_TARGET)
GO_CROSSBUILD_TARGETS += $(GO_CROSSBUILD_WINDOWS_ARM64_TARGET)

GO_BUILD_EXTLDFLAGS     := $(strip $(GO_BUILD_EXTLDFLAGS))
GO_BUILD_TAGS           := $(strip $(GO_BUILD_TAGS))
GO_BUILD_TARGET_DEPS    := $(strip $(GO_BUILD_TARGET_DEPS))
GO_BUILD_FLAGS          := $(strip $(GO_BUILD_FLAGS)) $(if $(GO_BUILD_TAGS),-tags $(GO_BUILD_TAGS))
GO_BUILD_LDFLAGS_OPTIMS := $(strip $(GO_BUILD_LDFLAGS_OPTIMS))
GO_BUILD_LDFLAGS        := -ldflags '$(GO_BUILD_LDFLAGS_OPTIMS) -X main.Version=$(GIT_VERSION) -extldflags "$(GO_BUILD_EXTLDFLAGS)"'

GO_TOOLS_GOLANGCI_LINT_VERSION ?= v2.1.6
GO_TOOLS_GOLANGCI_LINT         ?= $(shell $(GO) env GOPATH)/bin/golangci-lint-$(GO_TOOLS_GOLANGCI_LINT_VERSION)

# ------------------------------------------------------------------------------

.PHONY: all build install crossbuild crossbuild-checksums clean .FORCE

build: $(GO_BUILD_VERSION_TARGET) $(GO_BUILD_TARGET)

all: crossbuild crossbuild-checksums

install:
	CGO_ENABLED=$(CGO_ENABLED) $(GO) install $(GO_BUILD_FLAGS) $(GO_BUILD_LDFLAGS)

$(GO_BUILD_FLAGS_TARGET) : .FORCE
	@(echo "GO_VERSION=$(shell $(GO) version)"; \
	  echo "GO_GOOS=$(GOOS)"; \
	  echo "GO_GOARCH=$(GOARCH)"; \
	  echo "GO_GOARM=$(GOARM)"; \
	  echo "GO_BUILD_FLAGS=$(GO_BUILD_FLAGS)"; \
	  echo 'GO_BUILD_LDFLAGS=$(subst ','\'',$(GO_BUILD_LDFLAGS))'; \
	  echo "CGO_ENABLED=$(CGO_ENABLED)") \
	    | cmp -s - $@ \
	        || (echo "GO_VERSION=$(shell $(GO) version)"; \
	            echo "GO_GOOS=$(GOOS)"; \
	            echo "GO_GOARCH=$(GOARCH)"; \
	            echo "GO_GOARM=$(GOARM)"; \
	            echo "GO_BUILD_FLAGS=$(GO_BUILD_FLAGS)"; \
	            echo 'GO_BUILD_LDFLAGS=$(subst ','\'',$(GO_BUILD_LDFLAGS))'; \
	            echo "CGO_ENABLED=$(CGO_ENABLED)") > $@

$(GO_BUILD_TARGET): $(GO_BUILD_VERSION_TARGET)
	@(test -e $@ && unlink $@) || true
	@mkdir -p $$(dirname $@)
	@ln $< $@

$(GO_BUILD_VERSION_TARGET): $(GO_BUILD_SRC) $(GO_BUILD_FLAGS_TARGET) | $(GO_BUILD_TARGET_DEPS)
	CGO_ENABLED=$(CGO_ENABLED) GOOS=$(GOOS) GOARCH=$(GOARCH) GOARM=$(GOARM) $(GO) build $(GO_BUILD_FLAGS) $(GO_BUILD_LDFLAGS) -o $@

# coinbasemd is pure Go: cross builds need no C toolchain.
crossbuild: $(GO_BUILD_VERSION_TARGET) $(GO_CROSSBUILD_TARGETS)

$(GO_CROSSBUILD_LINUX_AMD64_TARGET): $(GO_BUILD_SRC) $(GO_BUILD_FLAGS_TARGET)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build $(GO_BUILD_FLAGS) $(GO_BUILD_LDFLAGS) -o $@

$(GO_CROSSBUILD_LINUX_ARM64_TARGET): $(GO_BUILD_SRC) $(GO_BUILD_FLAGS_TARGET)
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build $(GO_BUILD_FLAGS) $(GO_BUILD_LDFLAGS) -o $@

$(GO_CROSSBUILD_DARWIN_AMD64_TARGET): $(GO_BUILD_SRC) $(GO_BUILD_FLAGS_TARGET)
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 $(GO) build $(GO_BUILD_FLAGS) $(GO_BUILD_LDFLAGS) -o $@

$(GO_CROSSBUILD_DARWIN_ARM64_TARGET): $(GO_BUILD_SRC) $(GO_BUILD_FLAGS_TARGET)
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build $(GO_BUILD_FLAGS) $(GO_BUILD_LDFLAGS) -o $@

$(GO_CROSSBUILD_WINDOWS_AMD64_TARGET): $(GO_BUILD_SRC) $(GO_BUILD_FLAGS_TARGET)
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build $(GO_BUILD_FLAGS) $(GO_BUILD_LDFLAGS) -o $@

$(GO_CROSSBUILD_WINDOWS_ARM64_TARGET): $(GO_BUILD_SRC) $(GO_BUILD_FLAGS_TARGET)
	CGO_ENABLED=0 GOOS=windows GOARCH=arm64 $(GO) build $(GO_BUILD_FLAGS) $(GO_BUILD_LDFLAGS) -o $@

crossbuild-checksums: dist/checksums

dist/checksums : $(GO_CROSSBUILD_TARGETS)
	cd dist && shasum -a 256 $(notdir $(GO_CROSSBUILD_TARGETS)) > checksums

clean:
	rm -rf dist $(GO_BUILD_FLAGS_TARGET)

# -- go generate ---------------------------------------------------------------

.PHONY: generate generate-verify

# Regenerates internal/fixt and internal/md from Coinbase's dictionaries with fixgen.
generate:
	$(GO) generate ./...

generate-verify: generate
	git diff --exit-code -- internal/fixt internal/md

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
