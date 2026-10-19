#!/bin/sh
# The test as a job on the control host, run as root from the jepsen
# directory: started under systemd, so that it outlives the SSH session
# that started it; one at a time, and not while the suite builds
# (build.sh takes the same lock). NODES: the nodes' names.
#
#   job.sh start [lein run test options]   starts one, prints its id
#   job.sh wait ID                         follows its log until it ends
#   job.sh status ID                       its exit status, once ended
#   job.sh run ID                          its directory, under the store
#   job.sh stop                            stops the one running
#   job.sh last                            the last one's id
set -eu
jobs=/var/lib/fix2-jepsen/jobs
store=/var/lib/fix2-jepsen/store
lock=/var/lib/fix2-jepsen/lock

unit() { echo "fix2-jepsen-test-$1"; }
# running: whether a test runs, whichever, stopping included: its JVM may
# live on until stopped.
running() {
	systemctl list-units 'fix2-jepsen-test-*' --all --no-legend --plain |
		awk '$3 != "inactive" && $3 != "failed"' | grep -q .
}
# dir: a job's state, its id checked.
dir() {
	case ${1:-} in '' | *[!0-9a-z-]*) echo "no such job: ${1:-}" >&2; exit 2 ;; esac
	[ -d "$jobs/$1" ] || { echo "no such job: $1" >&2; exit 2; }
	echo "$jobs/$1"
}

# netdev is the nodes' interface to each other, from the routes between
# them: found in every direction, and the same, or the test cannot shape it.
netdev() {
	devs=
	set -- $NODES
	for n in "$@"; do
		for p in "$@"; do
			[ "$p" != "$n" ] || continue
			r=$(ssh "$n" "ip -o route get \$(getent ahostsv4 $p | awk '{print \$1; exit}')") ||
				{ echo "no route from $n to $p: give --net-dev" >&2; exit 1; }
			d=$(printf '%s\n' "$r" | sed -n 's/.* dev \([^ ]*\).*/\1/p')
			[ -n "$d" ] || { echo "no interface from $n to $p in: $r" >&2; exit 1; }
			case " $devs " in *" $d "*) ;; *) devs="$devs $d" ;; esac
		done
	done
	set -- $devs
	if [ $# -ne 1 ]; then
		echo "the nodes reach each other through several interfaces:$devs; give --net-dev" >&2
		exit 1
	fi
	echo "$1"
}

case ${1:-} in
start)
	shift
	case " $* " in *" --net-dev "*) dev= ;; *) dev=$(netdev) ;; esac
	mkdir -p "$jobs"
	exec 9>"$lock"
	flock -n 9 || { echo "a test is starting, or the suite building: start again later" >&2; exit 1; }
	if running; then
		echo "a test is running: vm-test-wait follows it, vm-test-stop stops it" >&2
		exit 1
	fi
	id=$(date -u +%Y%m%dt%H%M%S)-$(od -An -N3 -tx1 /dev/urandom | tr -d ' \n')
	d=$jobs/$id
	mkdir "$d"
	nodes=$(echo $NODES | tr ' ' ,)
	set -- test --nodes "$nodes" --username root --ssh-private-key /root/.ssh/id_rsa ${dev:+--net-dev "$dev"} "$@"
	printf '%s\n' "$@" >"$d/args"
	echo "$id" >"$jobs/last"
	# Active once it returns: the lock released, a build sees it.
	systemd-run --quiet --unit "$(unit "$id")" --collect --working-directory "$PWD" \
		--setenv LEIN_ROOT=true --setenv HOME=/root \
		sh "$PWD/infra/job.sh" run-job "$id"
	echo "started $id: lein run $*" >&2
	echo "$id"
	;;
run-job)
	# Under systemd: lein's status, and the directory the test wrote.
	d=$(dir "${2:-}")
	touch "$d/started"
	set --
	while IFS= read -r a; do set -- "$@" "$a"; done <"$d/args"
	status=0
	lein run "$@" >"$d/log" 2>&1 </dev/null || status=$?
	find "$store" -mindepth 2 -maxdepth 2 -type d -newer "$d/started" | sort | tail -1 |
		sed "s|^$store/||" >"$d/run"
	echo "$status" >"$d/exit"
	;;
wait)
	d=$(dir "${2:-}")
	touch "$d/log"
	tail -n +1 -F "$d/log" 2>/dev/null &
	t=$!
	while systemctl is-active --quiet "$(unit "$2")"; do sleep 2; done
	sleep 1
	kill $t
	;;
status)
	d=$(dir "${2:-}")
	cat "$d/exit" 2>/dev/null || { echo "no status: running, or stopped" >&2; exit 1; }
	;;
run)
	d=$(dir "${2:-}")
	r=$(cat "$d/run" 2>/dev/null || true)
	[ -n "$r" ] && [ -e "$store/$r/results.edn" ] || { echo "no results" >&2; exit 1; }
	echo "$r"
	;;
stop)
	systemctl stop 'fix2-jepsen-test-*' 2>/dev/null || true
	;;
last)
	cat "$jobs/last" 2>/dev/null || { echo "no test started" >&2; exit 1; }
	;;
*)
	sed -n '2,13s/^# \{0,1\}//p' "$0" >&2
	exit 2
	;;
esac
