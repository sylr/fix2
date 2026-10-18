#!/bin/sh
# Waits for the control node's public key, authorizes it for root, and runs
# sshd in the foreground.
set -eu
while [ ! -s /secret/id_rsa.pub ]; do sleep 0.2; done
cp /secret/id_rsa.pub /root/.ssh/authorized_keys
chmod 600 /root/.ssh/authorized_keys
exec /usr/sbin/sshd -D -e
