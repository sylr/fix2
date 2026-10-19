# Unpacks the sources jepsen-run copied to the control host and builds the
# node binary and the test's dependencies there, run as root. The results
# live outside the sources, which each build replaces. Given: ARCHIVE, and
# HASH, its digest.
set -eu
log=/var/log/fix2-jepsen-build.log
exec 3>&1 >"$log" 2>&1
say() { echo "$*" >&3; echo "== $*"; }
trap 'echo "failed: see $log on $(hostname)" >&3; tail -20 "$log" >&3' EXIT

d=/opt/fix2-jepsen
# The sources a running test reads are not replaced under it: the lock
# infra/job.sh takes to start one, held to the end of the build.
exec 9>/var/lib/fix2-jepsen/lock
if ! flock -n 9 || systemctl list-units 'fix2-jepsen-test-*' --all --no-legend --plain |
	awk '$3 != "inactive" && $3 != "failed"' | grep -q .; then
	echo "a test is running or starting: wait for it, or stop it (make vm-test-stop)" >&3
	exit 1
fi
say "sources"
rm -rf "$d/src.new"
mkdir -p "$d/src.new"
tar -xzf "$ARCHIVE" -C "$d/src.new"
rm -rf "$d/src"
mv "$d/src.new" "$d/src"
ln -s /var/lib/fix2-jepsen/store "$d/src/jepsen/store"

say "node binary"
mkdir -p "$d/src/jepsen/bin"
cd "$d/src/jepsen/node"
PATH=/usr/local/go/bin:$PATH GOTOOLCHAIN=local go build -trimpath -o ../bin/fix2-jepsen-node .

say "test dependencies"
cd "$d/src/jepsen"
LEIN_ROOT=true lein deps

trap - EXIT
say "built $HASH"
