GO  ?= go
MVN ?= mvn

# Agents are built the same way, without the race detector or debugging
# information that would weigh on one more than another.
GO_BUILD_FLAGS ?= -trimpath
MVN_FLAGS      ?= -q -B

# The driver's flags, see go run ./cmd/fixbench -h.
MESSAGES ?= 50000
WARMUP   ?= 50000
WINDOWS  ?= 1,10,100,1000
RUNS     ?= 5
VALIDATE ?= true
OUTPUT   ?= results.txt
BENCH_FLAGS ?=

# Every directory of agents/ is an agent: a Go one if it holds Go files, a
# Maven one if it holds a pom.xml. AGENTS chooses those to build and run.
GO_AGENTS   := $(sort $(patsubst agents/%/,%,$(dir $(wildcard agents/*/*.go))))
JAVA_AGENTS := $(patsubst agents/%/pom.xml,%,$(wildcard agents/*/pom.xml))
AGENTS      ?= $(GO_AGENTS) $(JAVA_AGENTS)
# The agent the others are compared to, one of AGENTS.
BASELINE    ?= fix2

.PHONY: build bench vet clean

build:
	mkdir -p bin
	$(GO) build $(GO_BUILD_FLAGS) -o bin/ ./cmd/fixbench $(addprefix ./agents/,$(filter $(GO_AGENTS),$(AGENTS)))
	for a in $(filter $(JAVA_AGENTS),$(AGENTS)); do \
		$(MVN) $(MVN_FLAGS) -f agents/$$a/pom.xml package && \
		cp agents/$$a/target/$$a-agent.jar bin/ && \
		install -m 755 agents/$$a/$$a.sh bin/$$a || exit 1; \
	done

# bench runs the matrix and writes every run to $(OUTPUT), for benchstat:
# go run golang.org/x/perf/cmd/benchstat@latest -col /initiator results.txt
bench: build
	bin/fixbench $(foreach a,$(AGENTS),-impl $(a)=bin/$(a)) -baseline $(BASELINE) \
		-messages $(MESSAGES) -warmup $(WARMUP) -windows $(WINDOWS) -runs $(RUNS) \
		-validate=$(VALIDATE) -o $(OUTPUT) $(BENCH_FLAGS)

vet:
	$(GO) vet ./...

clean:
	rm -rf bin $(addprefix agents/,$(addsuffix /target,$(JAVA_AGENTS)))
