GO    ?= go
MVN   ?= mvn
CMAKE ?= cmake

# Agents are built the same way, without the race detector or debugging
# information that would weigh on one more than another.
GO_BUILD_FLAGS ?= -trimpath
MVN_FLAGS      ?= -q -B
CMAKE_FLAGS    ?=

# The driver's flags, see go run ./cmd/fixbench -h.
MESSAGES ?= 50000
WARMUP   ?= 50000
WINDOWS  ?= 1,10,100,1000
RUNS     ?= 10
SCENARIOS ?= orders,marketdata
OUTPUT   ?= results.txt
BENCH_FLAGS ?=

# The dictionary every agent's code is generated from and validates with.
DICT := spec/FIX44.xml

# Every directory of agents/ is an agent: a Go one if it holds Go files, a
# Maven one if it holds a pom.xml, a CMake one if it holds a CMakeLists.txt.
# AGENTS chooses those to build and run.
GO_AGENTS    := $(sort $(patsubst agents/%/,%,$(dir $(wildcard agents/*/*.go))))
JAVA_AGENTS  := $(patsubst agents/%/pom.xml,%,$(wildcard agents/*/pom.xml))
CMAKE_AGENTS := $(patsubst agents/%/CMakeLists.txt,%,$(wildcard agents/*/CMakeLists.txt))
AGENTS       ?= $(GO_AGENTS) $(JAVA_AGENTS) $(CMAKE_AGENTS)
# The agent the others are compared to, one of AGENTS.
BASELINE    ?= fix2

# The CMake agents build in a directory of their toolchain's, the compiler
# and the QuickFIX C++ they build with, both of the shell (nix develop): a
# CMake cache keeps the compiler it was configured with, which a new
# toolchain must not inherit.
CXX_PATH       := $(shell command -v $(CXX))
CMAKE_BUILD_ID := $(shell printf '%s\n%s\n' '$(CXX_PATH)' '$(QUICKFIX_CPP)' | cksum | cut -d' ' -f1)

.PHONY: generate build bench vet clean

# generate generates the Go engines' code from $(DICT), into gen/: fix2's
# with fixgen, QuickFIX/Go's with its generate-fix, both Go tools of the
# module. The Java and C++ agents generate theirs as they build.
generate: gen/fix2/.stamp gen/quickfixgo/.stamp

# fixgen is built from the fix2 of the repository: its sources count, and
# go.mod, which pins QuickFIX/Go's generate-fix.
FIXGEN_SRC := $(shell find ../../cmd/fixgen ../../dictionary -name '*.go' ! -name '*_test.go')

gen/fix2/.stamp: $(DICT) go.mod go.sum $(FIXGEN_SRC)
	rm -rf gen/fix2 && mkdir -p gen/fix2/fix44
	$(GO) tool fixgen -dict $(DICT) -pkg fix44 -out gen/fix2/fix44 -dispatch
	touch $@

gen/quickfixgo/.stamp: $(DICT) go.mod go.sum
	rm -rf gen/quickfixgo && mkdir -p gen/quickfixgo
	cd gen/quickfixgo && $(GO) tool generate-fix -pkg-root github.com/sylr/fix2/tests/benchmarks/gen/quickfixgo $(abspath $(DICT))
	touch $@

build: generate
	mkdir -p bin
	$(GO) build $(GO_BUILD_FLAGS) -o bin/ ./cmd/fixbench $(addprefix ./agents/,$(filter $(GO_AGENTS),$(AGENTS)))
	for a in $(filter $(JAVA_AGENTS),$(AGENTS)); do \
		$(MVN) $(MVN_FLAGS) -f agents/$$a/pom.xml package && \
		cp agents/$$a/target/$$a-agent.jar bin/ && \
		install -m 755 agents/$$a/$$a.sh bin/$$a || exit 1; \
	done
	for a in $(filter $(CMAKE_AGENTS),$(AGENTS)); do \
		b=agents/$$a/build/$(CMAKE_BUILD_ID); \
		$(CMAKE) -S agents/$$a -B $$b -DCMAKE_BUILD_TYPE=Release -DCMAKE_CXX_COMPILER=$(CXX_PATH) $(CMAKE_FLAGS) && \
		$(CMAKE) --build $$b && \
		install -m 755 $$b/$$a bin/$$a || exit 1; \
	done

# bench runs the matrix and writes every run to $(OUTPUT), for benchstat:
# go run golang.org/x/perf/cmd/benchstat@latest -col /initiator results.txt
bench: build
	bin/fixbench $(foreach a,$(AGENTS),-impl $(a)=bin/$(a)) -baseline $(BASELINE) \
		-scenarios $(SCENARIOS) -messages $(MESSAGES) -warmup $(WARMUP) -windows $(WINDOWS) -runs $(RUNS) \
		-dict $(DICT) -o $(OUTPUT) $(BENCH_FLAGS)

vet: generate
	$(GO) vet ./...

clean:
	rm -rf bin gen $(addprefix agents/,$(addsuffix /target,$(JAVA_AGENTS)) $(addsuffix /build,$(CMAKE_AGENTS)))
