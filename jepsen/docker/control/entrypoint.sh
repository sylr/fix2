#!/bin/sh
# Makes the SSH key the nodes authorize, once, trusts the nodes' host keys,
# and stays up for `docker compose exec`.
set -eu
if [ ! -s /secret/id_rsa ]; then
	ssh-keygen -q -t rsa -b 4096 -N '' -f /secret/id_rsa.tmp
	mv /secret/id_rsa.tmp.pub /secret/id_rsa.pub
	mv /secret/id_rsa.tmp /secret/id_rsa
fi
mkdir -p /root/.ssh
chmod 700 /root/.ssh
cp /secret/id_rsa /root/.ssh/id_rsa
chmod 600 /root/.ssh/id_rsa
cat > /root/.ssh/config <<EOF
Host n1 n2
	User root
	StrictHostKeyChecking accept-new
EOF
rm -f /root/.ssh/known_hosts
for n in n1 n2; do
	until ssh-keyscan -t rsa,ed25519 "$n" >>/root/.ssh/known_hosts 2>/dev/null; do sleep 0.5; done
done
exec sleep infinity
