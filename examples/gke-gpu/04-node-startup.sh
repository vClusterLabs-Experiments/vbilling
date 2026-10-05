#!/bin/bash
# Startup script that joins a VM to a tenant cluster as a vCluster private node,
# once. Replace JOIN_COMMAND with the line `vcluster token create` prints, e.g.
#   curl -fsSLk "https://<endpoint>/node/join?token=<token>" | sh -
# The token lets machines join the tenant cluster until it expires: remove this
# script from the instance metadata after the node joined, and never commit it
# with a token in it.
set -euo pipefail
exec >> /var/log/vcluster-join.log 2>&1
[ -f /var/lib/vcluster-joined ] && { echo "already joined"; exit 0; }
echo "join started $(date -u +%FT%TZ)"

JOIN_COMMAND

touch /var/lib/vcluster-joined
echo "join finished $(date -u +%FT%TZ)"
