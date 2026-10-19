#!/bin/sh
# Runs the test as a job on the control host the jepsen-run stack $STACK
# set up (infra/job.sh), from the jepsen directory:
#
#   vm-test.sh start [lein run test options]   starts it, then as wait
#   vm-test.sh wait [ID]   follows it, the last one started unless given, to
#                          its end, fetches its results into store/, and
#                          exits with its verdict: 0 valid, 1 invalid,
#                          2 unknown; 3 if it reached none (stopped,
#                          crashed, or the connection lost: wait again),
#                          4 if its results could not be fetched
#   vm-test.sh fetch [ID]  fetches its results: 4 if it cannot
#   vm-test.sh stop        stops the one running
set -eu
on() { sh infra/on-control.sh "$@"; }

# fetch fetches the results of job $1, whole or not at all: extracted
# aside, then moved into store/ and made its latest; one fetch at a time,
# and a run fetched whole not fetched again.
fetch() {
	run=$(on "sh infra/job.sh run $1") || return 1
	if [ -e "store/$run/results.edn" ]; then
		# Made its latest still: a fetch interrupted may not have.
		ln -sfn "$(basename "$run")" "store/$(dirname "$run")/latest" || return 1
		echo "results: store/$run, fetched already"
		return 0
	fi
	mkdir -p store
	mkdir store/.fetch.lock 2>/dev/null ||
		{ echo "another fetch holds store/.fetch.lock: remove it if none runs" >&2; return 1; }
	tmp=$(mktemp -d store/.fetch.XXXXXX) || { rmdir store/.fetch.lock; return 1; }
	trap 'rm -rf "$tmp"; rmdir store/.fetch.lock' EXIT
	trap 'exit 130' INT TERM
	on "tar -C /var/lib/fix2-jepsen/store -czf - '$run'" >"$tmp/run.tar.gz" || return 1
	tar -C "$tmp" -xzf "$tmp/run.tar.gz" || return 1
	[ -e "$tmp/$run/results.edn" ] || { echo "fetched no results.edn" >&2; return 1; }
	# Called in a condition, set -e is off: each step checked.
	mkdir -p "store/$(dirname "$run")" || return 1
	rm -rf "store/$run" || return 1
	mv "$tmp/$run" "store/$run" || return 1
	ln -sfn "$(basename "$run")" "store/$(dirname "$run")/latest" || return 1
	rm -rf "$tmp"
	rmdir store/.fetch.lock
	trap - EXIT INT TERM
	echo "results: store/$run"
}

id() { [ -n "${1:-}" ] && echo "$1" || on 'sh infra/job.sh last'; }

case ${1:-} in
start)
	shift
	q=
	for a in "$@"; do q="$q '$(printf '%s' "$a" | sed "s/'/'\\\\''/g")'"; done
	id=$(on "sh infra/job.sh start$q")
	exec sh "$0" wait "$id"
	;;
wait)
	id=$(id "${2:-}") || { echo "no test to follow" >&2; exit 3; }
	on "sh infra/job.sh wait $id" || true
	if ! status=$(on "sh infra/job.sh status $id"); then
		echo "test $id reached no verdict: still running, the connection lost (wait again), or stopped" >&2
		exit 3
	fi
	# A verdict is Jepsen's status with results written: else the run
	# failed before checking, whatever its status.
	if ! on "sh infra/job.sh run $id" >/dev/null || [ "$status" -gt 2 ]; then
		echo "test $id reached no verdict: lein exited $status; its log: /var/lib/fix2-jepsen/jobs/$id/log on control" >&2
		exit 3
	fi
	if ! fetch "$id"; then
		echo "test $id: verdict $status, its results not fetched: vm-test-fetch" >&2
		exit 4
	fi
	exit "$status"
	;;
fetch)
	id=$(id "${2:-}") || { echo "no test to fetch" >&2; exit 3; }
	fetch "$id" || exit 4
	;;
stop) on 'sh infra/job.sh stop' ;;
*)
	sed -n '2,14s/^# \{0,1\}//p' "$0" >&2
	exit 2
	;;
esac
