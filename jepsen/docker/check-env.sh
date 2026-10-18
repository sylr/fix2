#!/bin/sh
# Checks, from the control node, what the suite needs of the environment:
# SSH to each node, the node binary running there, the nemeses' tools
# working (netem, iptables, loop devices), and LazyFS mounting, losing what
# was not fsynced and keeping what was, as jepsen.lazyfs drives it. Run by
# `make check-env`.
set -eu

for n in n1 n2; do
	ssh "$n" true
	echo "$n: ssh ok"
	scp -q /jepsen/bin/fix2-jepsen-node "$n":/tmp/fix2-jepsen-node
	# -h exits 1: the usage it prints shows the binary runs.
	usage=$(ssh "$n" '/tmp/fix2-jepsen-node -h 2>&1 || true')
	case "$usage" in
	"Usage of fix2-jepsen-node"*) echo "$n: binary ok" ;;
	*) echo "$n: binary does not run: $usage"; exit 1 ;;
	esac
	ssh "$n" 'set -eu
for c in iptables tc findmnt mountpoint fusermount fallocate losetup mkfs.ext4 start-stop-daemon pgrep; do
	command -v $c >/dev/null || { echo "missing $c"; exit 1; }
done
dev=$(ip -o route get 8.8.8.8 | sed -n "s/.* dev \([^ ]*\).*/\1/p")
tc qdisc add dev "$dev" root netem delay 1ms
tc qdisc del dev "$dev" root
iptables -w -A INPUT -s 192.0.2.1 -j DROP
iptables -w -D INPUT -s 192.0.2.1 -j DROP
img=$(mktemp)
fallocate -l 16M "$img"
mkfs.ext4 -q "$img"
loop=$(losetup -f --show "$img")
losetup -d "$loop"
rm -f "$img"
echo "tools ok: netem on $dev, iptables, loop devices"'
done

ssh n1 'set -eu
d=/tmp/lz l=/tmp/lz.lazyfs
fusermount -uz $d 2>/dev/null || true
rm -rf $d $l
mkdir -p $d $l/data
touch $l/log
cat >$l/config <<EOF
[faults]
fifo_path="$l/fifo"
[cache]
apply_eviction=false
[cache.simple]
custom_size="0.1GB"
blocks_per_page=1
[filesystem]
logfile="$l/log"
log_all_operations=false
EOF
cd /opt/jepsen/lazyfs/lazyfs
scripts/mount-lazyfs.sh -c $l/config -m $d -r $l/data >/dev/null
trap "fusermount -uz $d" EXIT
for i in $(seq 50); do findmnt $d | grep -q lazyfs && break; sleep 0.1; done
findmnt $d | grep -q lazyfs
echo kept >$d/synced && sync $d/synced
echo lost >$d/unsynced
echo lazyfs::clear-cache >$l/fifo
sleep 0.5
synced=$(cat $d/synced) unsynced=$(cat $d/unsynced)
if [ "$synced" != kept ]; then echo "lazyfs lost a synced write: \"$synced\""; exit 1; fi
if [ "$unsynced" = lost ]; then echo "lazyfs kept an unsynced write"; exit 1; fi
echo "lazyfs ok: the synced write kept, the unsynced one lost"'
