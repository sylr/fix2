GO       ?= go
VERBOSE  ?= 0
CODECOV  ?= 0
RACE     ?= 0
FUZZTIME ?= 30s

# Time to spend minimizing an input the fuzzer finds interesting or failing.
# FuzzGenerate and FuzzLoad, whose inputs are whole dictionaries, would spend
# all their time minimizing: they never do.
FUZZ_MINIMIZE_TIME  ?= 10s
FUZZ_NO_MINIMIZE    ?= FuzzGenerate FuzzLoad

# The contribs are modules of their own, tested and fuzzed apart: tools, and
# stores under contribs/store.
CONTRIBS := $(patsubst %/go.mod,%,$(wildcard contribs/*/go.mod contribs/*/*/go.mod))

GO_TEST_FLAGS ?=
GO_FUZZ_FLAGS ?=
ifneq ($(VERBOSE),0)
GO_TEST_FLAGS += -v
endif
ifneq ($(RACE),0)
GO_TEST_FLAGS += -race
endif
ifneq ($(CODECOV),0)
# Every package's tests count towards every package's coverage: the session
# tests cover the parser too.
GO_TEST_FLAGS += -coverprofile=coverage.txt -covermode=atomic -coverpkg=./...
endif

# -- test ----------------------------------------------------------------------

.PHONY: test test-contribs vet fuzz

test:
	$(GO) test $(GO_TEST_FLAGS) ./...

test-contribs:
	for dir in $(CONTRIBS); do (cd $$dir && $(GO) test $(GO_TEST_FLAGS) ./...) || exit 1; done

vet:
	$(GO) vet ./...
	for dir in $(CONTRIBS); do (cd $$dir && $(GO) vet ./...) || exit 1; done

# fuzz runs every fuzz target of the module and of the contribs for FUZZTIME
# each. A failing input is written to the package's testdata/fuzz.
fuzz:
	set -e; \
	for dir in . $(CONTRIBS); do \
		cd $(CURDIR)/$$dir; \
		pkgs=$$($(GO) list ./...); \
		for pkg in $$pkgs; do \
			targets=$$($(GO) test -list '^Fuzz' $$pkg); \
			for target in $$(echo "$$targets" | grep '^Fuzz' || true); do \
				minimize=$(FUZZ_MINIMIZE_TIME); \
				case " $(FUZZ_NO_MINIMIZE) " in *" $$target "*) minimize=0 ;; esac; \
				echo "== $$pkg $$target"; \
				$(GO) test -run '^$$' -fuzz "^$$target\$$" -fuzztime $(FUZZTIME) -fuzzminimizetime $$minimize $(GO_FUZZ_FLAGS) $$pkg; \
			done; \
		done; \
	done

# -- go generate ---------------------------------------------------------------

.PHONY: generate generate-verify

# The directories go generate writes to: those holding a go:generate
# directive, each generating into its own (--out .), the contribs apart.
# Found on disk, tracked or not.
GENERATED = $$(grep -rl --include='*.go' '^//go:generate' --exclude-dir=contribs --exclude-dir=.git --exclude-dir=dist . | xargs -n1 dirname | sort -u)

# generate regenerates the module's generated code with fix2gen: the FIX
# test dictionaries, the FIXP codecs, the SBE generator's packages.
generate:
	$(GO) generate ./...

# generate-verify regenerates the module's code and fails if that changed a
# tracked file or added one, then runs the contribs' generate-verify, each
# contrib whose Makefile has one: those with go:generate directives.
generate-verify: generate
	git diff --exit-code -- $(GENERATED)
	@untracked=$$(git status --porcelain --untracked-files=all -- $(GENERATED) | grep '^??'); \
	if [ -n "$$untracked" ]; then echo "generated files not tracked:"; echo "$$untracked"; exit 1; fi
	for dir in $(CONTRIBS); do \
		if grep -q '^generate-verify:' $$dir/Makefile 2>/dev/null; then $(MAKE) -C $$dir generate-verify || exit 1; fi; \
	done

# -- go mod --------------------------------------------------------------------

.PHONY: go-mod-verify go-mod-tidy

go-mod-verify: go-mod-tidy
	git diff --quiet -- go.mod go.sum $(addsuffix /go.*,$(CONTRIBS)) || git diff --exit-code -- go.mod go.sum $(addsuffix /go.*,$(CONTRIBS))

go-mod-tidy:
	$(GO) mod tidy
	for dir in $(CONTRIBS); do (cd $$dir && $(GO) mod tidy) || exit 1; done
