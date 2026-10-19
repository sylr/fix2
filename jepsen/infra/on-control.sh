#!/bin/sh
# Runs a shell command on the control host the jepsen-run stack $STACK set
# up, in its jepsen directory, NODES set to the nodes' names: on-control.sh
# [-t] 'command'. -t allocates a terminal, for a console.
set -eu
: "${STACK:=dev}" "${PULUMI_CONFIG_PASSPHRASE=}"
export PULUMI_CONFIG_PASSPHRASE
here=$(cd "$(dirname "$0")" && pwd)
tty=
if [ "${1:-}" = -t ]; then tty=-t; shift; fi
out() { pulumi stack output --stack "$STACK" --cwd "$here/run" "$@"; }
control=$(out control)
user=$(out user)
dir=$(out jepsenDir)
nodes=$(out nodes | tr , ' ')
key=$(mktemp)
trap 'rm -f "$key"' EXIT
out --show-secrets privateKey >"$key"
sudo=
[ "$user" = root ] || sudo='sudo '
# q quotes its argument for the remote shell.
q() { printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"; }
# The host keys change with the machines: the key authorizes nothing else.
ssh $tty -i "$key" -o IdentitiesOnly=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR \
	"$user@$control" "${sudo}env NODES=$(q "$nodes") LEIN_ROOT=true sh -c $(q "cd $(q "$dir") && $1")"
