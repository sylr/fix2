# Sets up the Jepsen control host of the fix2 suite on Debian or Ubuntu,
# run as root by jepsen-run: a JDK, Leiningen and the reports' tools, Go
# and a C compiler to build the node binary (cgo, for SQLite), the key the
# nodes authorize, their host keys, and the suite's hosts. Idempotent.
# Given: HOSTS (/etc/hosts lines), NODES (their names), JEPSEN_KEY (a
# private key), GO_VERSION, LEIN_VERSION, UPLOAD_USER (the SSH user).
set -eu
export DEBIAN_FRONTEND=noninteractive
log=/var/log/fix2-jepsen-setup.log
exec 3>&1 >>"$log" 2>&1
say() { echo "$*" >&3; echo "== $*"; }
trap 'echo "failed: see $log on $(hostname)" >&3; tail -20 "$log" >&3' EXIT

say "packages"
apt-get -qy update
apt-get -qy install --no-install-recommends \
	build-essential ca-certificates curl default-jdk-headless git gnuplot-nox graphviz libjna-java \
	openssh-client rsync tar

if [ "$(/usr/local/go/bin/go env GOVERSION 2>/dev/null)" != "go$GO_VERSION" ]; then
	say "Go $GO_VERSION"
	arch=$(dpkg --print-architecture)
	curl -fsSL -o /tmp/go.tar.gz "https://go.dev/dl/go$GO_VERSION.linux-$arch.tar.gz"
	rm -rf /usr/local/go
	tar -C /usr/local -xzf /tmp/go.tar.gz
	rm -f /tmp/go.tar.gz
fi

if ! LEIN_ROOT=true /usr/local/bin/lein version 2>/dev/null | grep -q "Leiningen $LEIN_VERSION "; then
	say "Leiningen $LEIN_VERSION"
	curl -fsSL -o /usr/local/bin/lein "https://raw.githubusercontent.com/technomancy/leiningen/$LEIN_VERSION/bin/lein"
	chmod +x /usr/local/bin/lein
	LEIN_ROOT=true /usr/local/bin/lein version
fi

say "access"
sed -i '/^# fix2-jepsen begin$/,/^# fix2-jepsen end$/d' /etc/hosts
printf '# fix2-jepsen begin\n%s\n# fix2-jepsen end\n' "$HOSTS" >>/etc/hosts
install -d -m 700 /root/.ssh
printf '%s\n' "$JEPSEN_KEY" >/root/.ssh/id_rsa
chmod 600 /root/.ssh/id_rsa
cat >/root/.ssh/config <<EOF
Host $NODES
	User root
	StrictHostKeyChecking accept-new
EOF
# The nodes' host keys anew: a node made again has others.
rm -f /root/.ssh/known_hosts
for n in $NODES; do
	i=0
	until ssh-keyscan -t rsa,ed25519 "$n" >>/root/.ssh/known_hosts 2>/dev/null; do
		i=$((i + 1))
		[ "$i" -lt 60 ] || { echo "no host key from $n"; exit 1; }
		sleep 1
	done
done

install -d /opt/fix2-jepsen /var/lib/fix2-jepsen/store
# Where the sources are copied to, by the SSH user, without sudo.
# What another user copied there is the SSH user's to replace.
install -d -o "$UPLOAD_USER" -m 700 /var/lib/fix2-jepsen/upload
chown -R "$UPLOAD_USER" /var/lib/fix2-jepsen/upload
trap - EXIT
say "ready"
