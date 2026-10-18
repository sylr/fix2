# The Jepsen suite of docs/jepsen.md, on Docker: a control node and two fix2
# nodes. Results go to store/.

COMPOSE := docker compose -f docker/compose.yaml
GO_IMAGE ?= golang:1.27-trixie
TIME_LIMIT ?= 60
CONCURRENCY ?= 4
TEST_OPTS ?=

.PHONY: images up down console node-binary check-env test web

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

# web serves the results on http://localhost:8080.
web:
	$(COMPOSE) exec -d control lein run serve --host 0.0.0.0 --port 8080

# check-env checks the environment works: the control node reaches both
# nodes over SSH, the node binary and the nemeses' tools run there, and
# LazyFS loses what was not fsynced.
check-env: node-binary
	$(COMPOSE) exec control sh /jepsen/docker/check-env.sh
