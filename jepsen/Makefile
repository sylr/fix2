# The Jepsen suite of docs/jepsen.md, on Docker: a control node and two fix2
# nodes. Results go to store/. The vm- targets run it on machines of its
# own instead.

COMPOSE := docker compose -f docker/compose.yaml
GO_IMAGE ?= golang:1.27-trixie
TIME_LIMIT ?= 60
CONCURRENCY ?= 4
TEST_OPTS ?=

.PHONY: images up down console node-binary check-env test unit verdicts web

# images builds the control and node images.
images:
	$(COMPOSE) build

# up starts the control node and the fix2 nodes. The control node is
# restarted to learn the nodes' host keys anew: a rebuilt node image has
# others.
up:
	$(COMPOSE) up -d
	$(COMPOSE) restart control

down:
	$(COMPOSE) down -v

console:
	$(COMPOSE) exec control bash

# node-binary builds fix2-jepsen-node for the nodes, in a Go container of
# their Debian: it links SQLite through cgo.
node-binary:
	docker run --rm -v "$(abspath ..):/src" -v fix2-jepsen-gomod:/go/pkg/mod -v fix2-jepsen-gocache:/root/.cache/go-build \
		-w /src/jepsen/node $(GO_IMAGE) go build -trimpath -o /src/jepsen/bin/fix2-jepsen-node .

# test runs the suite, the binary rebuilt first. TEST_OPTS takes the
# options of `lein run test`: --rate, --nemesis and the like.
test: node-binary
	$(COMPOSE) exec control lein run test --nodes n1,n2 --username root \
		--ssh-private-key /root/.ssh/id_rsa --concurrency $(CONCURRENCY) \
		--time-limit $(TIME_LIMIT) $(TEST_OPTS)

# unit runs the checkers' tests, on histories made to break each check.
unit:
	$(COMPOSE) exec control lein test

# verdicts prints each check's verdict of the last test, or of RESULTS: the
# store names a test's directory after its faults.
RESULTS ?= $(shell ls -td store/fix2*/latest 2>/dev/null | head -1)/results.edn
verdicts:
	$(COMPOSE) exec control lein run -m jepsen.fix2.verdicts $(RESULTS)

# reanalyze checks the last test, or RUN (a test's directory), again with
# the current checker.
RUN ?= $(shell ls -td store/fix2*/latest 2>/dev/null | head -1)
.PHONY: reanalyze
reanalyze:
	$(COMPOSE) exec control lein run -m jepsen.fix2.reanalyze $(RUN)

# web serves the results on http://localhost:8080.
web:
	$(COMPOSE) exec -d control lein run serve --host 0.0.0.0 --port 8080

# check-env checks the environment works: the control node reaches both
# nodes over SSH, the node binary and the nemeses' tools run there, and
# LazyFS loses what was not fsynced.
check-env: node-binary
	$(COMPOSE) exec control sh check-env.sh

# The suite on machines of its own (docs/jepsen.md): infra/multipass makes
# them as Multipass VMs on this machine, infra/run sets up hosts reachable
# over SSH and builds the suite on the control host. STACK names the stack
# of each; the stacks' secrets are encrypted with PULUMI_CONFIG_PASSPHRASE,
# empty unless set.
STACK ?= dev
export STACK
export PULUMI_CONFIG_PASSPHRASE ?=
ON_CONTROL := sh infra/on-control.sh

.PHONY: vm-up vm-deploy vm-down vm-check-env vm-unit vm-console

# vm-up makes the VMs, then sets them up and builds the suite there.
vm-up:
	cd infra/multipass && pulumi stack select --create $(STACK) && pulumi up --yes
	cd infra/run && pulumi stack select --create $(STACK) && \
		pulumi config set inventory organization/fix2-jepsen-multipass/$(STACK) && pulumi up --yes

# vm-deploy sets up and builds again, after a change to the sources, or on
# hosts of another kind: infra/run's config then holds them (hosts,
# privateKey) and no inventory.
vm-deploy:
	cd infra/run && pulumi stack select --create $(STACK) && pulumi up --yes

vm-down:
	cd infra/run && pulumi stack select $(STACK) && pulumi destroy --yes
	cd infra/multipass && pulumi stack select $(STACK) && pulumi destroy --yes

vm-check-env:
	$(ON_CONTROL) 'sh check-env.sh'

vm-unit:
	$(ON_CONTROL) 'lein test'

vm-console:
	$(ON_CONTROL) -t 'bash -l'
