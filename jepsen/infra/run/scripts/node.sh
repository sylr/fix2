# Sets up a Jepsen node of the fix2 suite on Debian or Ubuntu, run as root
# by jepsen-run: the tools the nemeses use (gcc and ntpdate for Jepsen's
# clock nemesis, dmsetup for the device-mapper disks), the kernel modules
# they need,
# LazyFS where jepsen.lazyfs looks for it, the control host's key for root,
# and the suite's hosts. Idempotent. Given: HOSTS (/etc/hosts lines),
# CONTROL_KEY (an authorized_keys line), LAZYFS_VERSION.
set -eu
export DEBIAN_FRONTEND=noninteractive
log=/var/log/fix2-jepsen-setup.log
exec 3>&1 >>"$log" 2>&1
say() { echo "$*" >&3; echo "== $*"; }
trap 'echo "failed: see $log on $(hostname)" >&3; tail -20 "$log" >&3' EXIT

say "packages"
apt-get -qy update
apt-get -qy install --no-install-recommends \
	ca-certificates cmake curl dmsetup e2fsprogs fuse3 g++ gcc git iproute2 iptables iputils-ping libfuse3-dev make \
	netcat-openbsd ntpsec-ntpdate openssh-server pkg-config procps psmisc sqlite3 sudo tar util-linux

# Packet faults need netem, a prio qdisc and u32 filters; some kernels
# keep them in a package of their own.
say "kernel modules"
mods="sch_netem sch_prio cls_u32 fuse loop"
missing=
for m in $mods; do modprobe "$m" || missing=yes; done
if [ -n "$missing" ]; then
	apt-get -qy install "linux-modules-extra-$(uname -r)"
	for m in $mods; do modprobe "$m"; done
fi

if [ "$(cat /opt/jepsen/lazyfs/.fix2-version 2>/dev/null)" != "$LAZYFS_VERSION" ]; then
	say "LazyFS $LAZYFS_VERSION"
	rm -rf /opt/jepsen/lazyfs
	git clone --branch "$LAZYFS_VERSION" --depth 1 https://github.com/dsrhaslab/lazyfs.git /opt/jepsen/lazyfs
	# The TOML library LazyFS fetches asks for a CMake older than 3.5,
	# which CMake 4 refuses unless told otherwise.
	export CMAKE_POLICY_VERSION_MINIMUM=3.5
	(cd /opt/jepsen/lazyfs/libs/libpcache && ./build.sh)
	(cd /opt/jepsen/lazyfs/lazyfs && ./build.sh)
	echo "$LAZYFS_VERSION" >/opt/jepsen/lazyfs/.fix2-version
fi
sed -i '/\s*user_allow_other/s/^#//g' /etc/fuse.conf

say "access"
install -d -m 700 /root/.ssh
touch /root/.ssh/authorized_keys
# The control host's key replaces the one before it; other keys stay.
{
	grep -v ' fix2-jepsen-control$' /root/.ssh/authorized_keys || true
	printf '%s fix2-jepsen-control\n' "$CONTROL_KEY"
} >/root/.ssh/authorized_keys.new
chmod 600 /root/.ssh/authorized_keys.new
mv /root/.ssh/authorized_keys.new /root/.ssh/authorized_keys

# The suite's hosts, between markers, replaced on every run.
sed -i '/^# fix2-jepsen begin$/,/^# fix2-jepsen end$/d' /etc/hosts
printf '# fix2-jepsen begin\n%s\n# fix2-jepsen end\n' "$HOSTS" >>/etc/hosts

trap - EXIT
say "ready"
