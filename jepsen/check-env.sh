#!/bin/sh
# Checks, from the control node, what the suite needs of the environment,
# on every node: SSH, the node binary running there, the nemeses' tools
# working as Jepsen drives them (a prio qdisc with netem under it and a u32
# filter on the interface to the other node, iptables, a loop-mounted
# ext4), and LazyFS mounting, losing what was not fsynced and keeping what
# was, as jepsen.lazyfs drives it. Run from the jepsen directory by `make
# check-env` (Docker) or `make vm-check-env`. NODES: the nodes' names; BIN:
# the node binary.
set -eu
NODES=${NODES:-n1 n2}
BIN=${BIN:-bin/fix2-jepsen-node}

# peer: the node after n, the first after the last.
peer() {
	first= next= found=
	for x in $NODES; do
		[ -n "$first" ] || first=$x
		[ -z "$found" ] || [ -n "$next" ] || next=$x
		[ "$x" != "$1" ] || found=yes
	done
	echo "${next:-$first}"
}

for n in $NODES; do
	p=$(peer "$n")
	ssh "$n" true
	echo "$n: ssh ok"
	scp -q "$BIN" "$n":/tmp/fix2-jepsen-node
	# -h exits 1: the usage it prints shows the binary runs.
	usage=$(ssh "$n" '/tmp/fix2-jepsen-node -h 2>&1 || true')
	case "$usage" in
	"Usage of fix2-jepsen-node"*) echo "$n: binary ok" ;;
	*) echo "$n: binary does not run: $usage"; exit 1 ;;
	esac
	ssh "$n" "set -eu
for c in iptables tc findmnt mountpoint umount fallocate losetup mkfs.ext4 start-stop-daemon pgrep; do
	command -v \$c >/dev/null || { echo \"missing \$c\"; exit 1; }
done
ip=\$(getent ahostsv4 $p | awk '{print \$1; exit}')
dev=\$(ip -o route get \"\$ip\" | sed -n 's/.* dev \([^ ]*\).*/\1/p')
tc qdisc del dev \"\$dev\" root 2>/dev/null || true
tc qdisc add dev \"\$dev\" root handle 1: prio bands 4 priomap 1 2 2 2 1 2 0 0 1 1 1 1 1 1 1 1
tc qdisc add dev \"\$dev\" parent 1:4 handle 40: netem delay 1ms
tc filter add dev \"\$dev\" parent 1:0 protocol ip prio 1 u32 match ip dst \"\$ip\" flowid 1:4
tc qdisc del dev \"\$dev\" root
iptables -w -A INPUT -s \"\$ip\" -j DROP
iptables -w -D INPUT -s \"\$ip\" -j DROP
img=\$(mktemp) mnt=\$(mktemp -d)
fallocate -l 16M \"\$img\"
mkfs.ext4 -q -F -m 0 \"\$img\"
mount -o loop \"\$img\" \"\$mnt\"
echo ok >\"\$mnt/f\" && sync \"\$mnt/f\"
umount \"\$mnt\"
rm -rf \"\$img\" \"\$mnt\"
echo \"$n: tools ok: netem, prio and u32 on \$dev to $p, iptables, a loop-mounted ext4\""
done

for n in $NODES; do
	# Where the suite mounts the store, and unmounted as it does: AppArmor
	# may allow FUSE unmounts under /tmp and not there.
	ssh "$n" 'set -eu
d=/var/lib/fix2-check/lz l=/var/lib/fix2-check/lz.lazyfs
! mountpoint -q $d || umount -l $d
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
trap "! mountpoint -q $d || umount -l $d" EXIT
for i in $(seq 50); do findmnt $d | grep -q lazyfs && break; sleep 0.1; done
findmnt $d | grep -q lazyfs
# dd fsyncs the file; `sync FILE` does not everywhere: uutils calls sync(2),
# which LazyFS ignores.
echo kept | dd of=$d/synced conv=fsync status=none
echo lost >$d/unsynced
echo lazyfs::clear-cache >$l/fifo
sleep 0.5
synced=$(cat $d/synced) unsynced=$(cat $d/unsynced)
if [ "$synced" != kept ]; then echo "lazyfs lost a synced write: \"$synced\""; exit 1; fi
if [ "$unsynced" = lost ]; then echo "lazyfs kept an unsynced write"; exit 1; fi
trap - EXIT
umount -l $d
if mountpoint -q $d; then echo "lazyfs not unmounted"; exit 1; fi
rm -rf /var/lib/fix2-check'
	echo "$n: lazyfs ok: the synced write kept, the unsynced one lost, unmounted"
done

# Device-mapper's delay and flakey targets, for slow-disk and flaky-disk:
# not everywhere (not on Docker Desktop), so told, not required.
for n in $NODES; do
	if ! ssh "$n" 'modprobe -a dm_delay dm_flakey 2>/dev/null && command -v dmsetup >/dev/null'; then
		echo "$n: device-mapper delay and flakey unavailable: slow-disk and flaky-disk cannot run"
		continue
	fi
	ssh "$n" 'set -eu
img=$(mktemp) name=fix2-check-dm
fallocate -l 16M "$img"
l=$(losetup -f --show "$img")
s=$(blockdev --getsz "$l")
trap "dmsetup remove $name 2>/dev/null || true; losetup -d $l; rm -f $img" EXIT
dmsetup create $name --table "0 $s delay $l 0 0 $l 0 300"
t0=$(date +%s%N)
dd if=/dev/zero of=/dev/mapper/$name bs=4096 count=1 oflag=direct,dsync status=none
ms=$(( ($(date +%s%N) - t0) / 1000000 ))
[ "$ms" -ge 250 ] || { echo "dm-delay delayed a write $ms ms, not 300"; exit 1; }
dmsetup suspend --nolockfs $name
dmsetup load $name --table "0 $s flakey $l 0 1 4 1 error_writes"
dmsetup resume $name
ok=0 failed=0
for i in $(seq 12); do
	if dd if=/dev/zero of=/dev/mapper/$name bs=4096 count=1 oflag=direct status=none 2>/dev/null; then ok=$((ok+1)); else failed=$((failed+1)); fi
	sleep 0.5
done
[ "$failed" -gt 0 ] && [ "$ok" -gt 0 ] || { echo "dm-flakey: $ok writes taken, $failed failed"; exit 1; }
echo "dm-delay held a write $ms ms; dm-flakey took $ok writes, failed $failed"'
	echo "$n: device-mapper ok"
done
