#!/usr/bin/env bash
# End-to-end test for losing the data volume, on a laptop.
#
# Builds a kind control plane cluster with one vCluster and a workload,
# installs vBilling from this repository's Helm chart with an archive volume,
# which also holds the ledger mirror, then:
#   - checks the mirror follows the ledger and verifies on its own;
#   - pushes usage and waits until the mirror holds it;
#   - kills the pod and deletes its data volume while the node takes no new
#     pods for 80 seconds, so a new pod starts later on an empty volume;
#   - checks vBilling restored itself before it metered anything: the same
#     metering source identity, a ledger that verifies, every event recorded
#     before the loss under the same ID and none twice, the windows of the
#     outage metered, delivery continuing from its cursor;
#   - pushes again what was pushed in the last second before the loss, as a
#     sender that retries does, and checks it is counted exactly once;
#   - checks `vbilling ledger restore` refuses a directory that holds a ledger;
#   - moves the ledger to another storage class the documented way (a clean
#     stop, a new claim, an upgrade) and checks nothing was lost on the way;
#   - turns the mirror off and checks source health says so.
#
# Usage is measured from the Kubernetes API (pod requests, lifetimes), plus
# one metric pushed to the ingest API. No Prometheus, gateway or GPU is used.
#
# MIRROR_VOLUME=1 runs the same checks with the mirror on a volume of its own
# (persistence.mirror, LEDGER_MIRROR_DIR) instead of the archive volume.
#
# Usage: scripts/e2e-restore.sh [--keep]   (--keep leaves everything running)
# Requires: docker, kind, kubectl, helm, vcluster (0.30+), jq, python3, curl.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
WORK=${WORK:-$(mktemp -d -t vbilling-restore)}
CLUSTER=vbilling-e2e-restore
KEEP=${1:-}
API=127.0.0.1:28201
TOKEN=e2e-admin-token # the API always needs a token
INGEST=e2e-ingest-token
MIRROR_VOLUME=${MIRROR_VOLUME:-}
if [ -n "$MIRROR_VOLUME" ]; then
  MIRROR=/var/lib/vbilling-mirror/vbilling-0
else
  MIRROR=/var/lib/vbilling-archive/vbilling-0/mirror
fi
export KUBECONFIG=$WORK/kubeconfig # never touches ~/.kube/config

pass=0; fail=0
ok()    { echo "  PASS  $*"; pass=$((pass+1)); }
bad()   { echo "  FAIL  $*"; fail=$((fail+1)); }
step()  { echo; echo "==> $*"; }
check() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }
api()   { curl -fsS -H "Authorization: Bearer $TOKEN" "http://$API$1"; }
vc()    { vcluster connect team-a -n vc-team-a --driver helm -- "$@"; }
minute(){ python3 -I -c 'import datetime,sys; t=datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(seconds=int(sys.argv[1])); print(t.replace(second=0,microsecond=0).strftime("%Y-%m-%dT%H:%M:%SZ"))' "${1:-0}"; }
wait_for() { local end=$((SECONDS+$1)); until eval "$2"; do [ $SECONDS -lt $end ] || return 1; sleep 3; done; }
ready()   { api /api/v1/status 2>/dev/null | jq -e '.ready' >/dev/null; }
running() { kubectl -n vc-team-a get pods -l vcluster.loft.sh/managed-by=team-a --no-headers 2>/dev/null | grep -c Running || true; }
window_done() { api /api/v1/status 2>/dev/null | jq -e --arg e "$1" '.ledger.watermarks.collector >= $e' >/dev/null; }
committed() { api /api/v1/status | jq -r '.ledger.committed'; }
identity()  { api /api/v1/status | jq -r '.metering_source'; }
mirror()    { api /api/v1/sources | jq -c '.sources[] | select(.name=="ledger-mirror")'; }
# The mirror holds everything the ledger committed: its source says "in step".
in_step()   { mirror | jq -e '.status=="ok" and (.message | startswith("in step with the ledger"))' >/dev/null; }
# The ledger sequence number of the mirror's recovery point, and whether it
# has reached a given one.
mirror_seq(){ mirror | jq -r '.message' | sed -n 's/.*ledger seq \([0-9]*\).*/\1/p'; }
holds()     { [ "$(mirror | jq -r .status)" = ok ] && [ "$(mirror_seq)" -ge "$1" ] 2>/dev/null; }
lag()       { api /metrics | awk '$1=="vbilling_ledger_mirror_lag_seconds" {print $2}'; }
# The pod's log in a file: grep -q on a pipe would end kubectl early, and
# with pipefail that reads as a failure.
logs()      { kubectl -n vbilling logs vbilling-0 >"$WORK/pod.log" 2>/dev/null || true; cat "$WORK/pod.log"; }
logged()    { logs >/dev/null; grep -q "$1" "$WORK/pod.log"; }
uid()       { kubectl -n vbilling get "$1" -o jsonpath='{.metadata.uid}' 2>/dev/null; }
# push <id> <quantity> <window start>: one window total, as a sender does.
push() {
  curl -sS -X POST "http://$API/api/v1/events" -H "Authorization: Bearer $INGEST" -H 'X-VBilling-Client: batch-scheduler' -H 'Content-Type: application/json' \
    -d "{\"events\":[{\"id\":\"$1\",\"tenant\":\"acme\",\"metric\":\"batch_jobs\",\"quantity\":$2,\"window_start\":\"$3\",\"window_end\":\"$(python3 -I -c 'import datetime,sys; print((datetime.datetime.fromisoformat(sys.argv[1].replace("Z","+00:00"))+datetime.timedelta(minutes=1)).strftime("%Y-%m-%dT%H:%M:%SZ"))' "$3")\"}]}"
}
events()    { api "/api/v1/events?from=$START&to=$(minute 120)&limit=100000"; }

PIDS=()
cleanup() {
  for p in "${PIDS[@]:-}"; do kill "$p" 2>/dev/null || true; done
  if [ "$KEEP" != "--keep" ]; then
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
    rm -rf "$WORK"
  else
    echo "Kept: KUBECONFIG=$KUBECONFIG, API http://$API"
  fi
}
trap cleanup EXIT
forward() {
  for p in "${PIDS[@]:-}"; do kill "$p" 2>/dev/null || true; done
  PIDS=()
  kubectl -n vbilling port-forward svc/vbilling "${API##*:}":8080 >"$WORK/pf.log" 2>&1 & PIDS+=($!)
  wait_for 30 "curl -s -o /dev/null http://$API/" || true
}
# The pod is replaced several times: wait for the new one, then reach it.
reconnect() {
  kubectl -n vbilling wait --for=condition=Ready pod/vbilling-0 --timeout=240s >/dev/null 2>&1 || true
  forward
  wait_for 180 ready
}

step "Cluster, vCluster, workload"
kind create cluster --name "$CLUSTER" --wait 120s >/dev/null
kind get kubeconfig --name "$CLUSTER" >"$KUBECONFIG"
docker build -q -t vbilling:e2e-restore "$ROOT" >/dev/null
kind load docker-image vbilling:e2e-restore --name "$CLUSTER" >/dev/null
vcluster create team-a -n vc-team-a --driver helm --connect=false >/dev/null
kubectl label ns vc-team-a vbilling.vcluster.com/tenant=acme --overwrite >/dev/null
cat >"$WORK/workload.yaml" <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata: {name: trainer}
spec:
  selector: {matchLabels: {app: trainer}}
  template:
    metadata: {labels: {app: trainer}}
    spec:
      containers:
        - name: trainer
          image: busybox:1.36
          command: [sh, -c, "while true; do sleep 5; done"]
          resources: {requests: {cpu: 500m, memory: 128Mi}}
EOF
vc kubectl apply -f "$WORK/workload.yaml" >/dev/null
check "the workload runs (synced to the control plane cluster)" "wait_for 180 '[ \"\$(running)\" -ge 1 ]'"

step "vBilling with an archive volume: the mirror follows the ledger"
cat >"$WORK/values.yaml" <<EOF
image: {repository: vbilling, tag: e2e-restore, pullPolicy: Never}
adapters: [noop]
region: e2e
metering:
  cpuMemoryBasis: requests
  customMetrics:
    - {code: batch_jobs, unit: jobs}
persistence:
  enabled: true
  size: 1Gi
  archive: {enabled: true, size: 1Gi}
$( [ -n "$MIRROR_VOLUME" ] && echo "  mirror: {enabled: true, size: 1Gi}" )
api: {token: $TOKEN, ingestToken: $INGEST}
EOF
helm install vbilling "$ROOT/deploy/helm/vbilling" -n vbilling --create-namespace -f "$WORK/values.yaml" --wait --timeout 240s >/dev/null
forward
check "vBilling is ready" "wait_for 120 ready"
START=$(minute 0)
check "two full windows are metered" "wait_for 240 'window_done $(minute 180)'"
check "the mirror is in step with the ledger" "wait_for 60 in_step"
echo "  $(mirror | jq -r '.message')"
check "the mirror's lag is exported and under 5 seconds" "python3 -I -c 'import sys; sys.exit(not (0 <= float(sys.argv[1]) < 5))' \"\$(lag)\""
kubectl -n vbilling exec vbilling-0 -- /vbilling ledger verify --dir "$MIRROR" >"$WORK/mirror-verify.json" 2>"$WORK/mirror-verify.err" || true
check "the mirror verifies on its own, with its recovery point" "jq -e '.ok and .mirror.ok and (.mirror.ledgers | length) == 2 and .mirror.state_files > 0' '$WORK/mirror-verify.json' >/dev/null"
kubectl -n vbilling exec vbilling-0 -- /vbilling ledger restore >"$WORK/refused.out" 2>&1 && REFUSED=0 || REFUSED=$?
check "ledger restore refuses the directory of the running ledger" "[ \"$REFUSED\" != 0 ] && grep -q 'already holds a ledger' '$WORK/refused.out' && [ \"\$(api /api/v1/ledger/verify | jq .ok)\" = true ]"

step "Pushed usage reaches the mirror"
PUSH_WINDOW=$(minute -120)
check "a pushed window total is accepted" "push job-early 7 $PUSH_WINDOW | jq -e '.accepted == 1' >/dev/null"
AT=$(committed)
check "the mirror holds it within 15 seconds (ledger seq $AT)" "wait_for 15 'holds $AT'"
ID=$(identity)
events >"$WORK/before.jsonl"
BEFORE_COMMITTED=$(committed)
DATA_PVC=$(uid pvc/data-vbilling-0); ARCHIVE_PVC=$(uid pvc/archive-vbilling-0); POD=$(uid pod/vbilling-0)
echo "  metering source $ID, ledger seq $BEFORE_COMMITTED, $(grep -c '^{' "$WORK/before.jsonl") events"

step "The data volume is lost"
# Pushed in the last second: it may or may not have reached the mirror.
push job-last-second 3 "$PUSH_WINDOW" >"$WORK/last-second.json" || true
# The node takes no new pods for a while, as when its replacement has to
# come up first: the outage lasts longer than a window, so there are windows
# to meter afterwards. The pod is killed, not stopped: no last mirror pass.
NODE=$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')
kubectl cordon "$NODE" >/dev/null
kubectl -n vbilling delete pvc data-vbilling-0 --wait=false >/dev/null
kubectl -n vbilling delete pod vbilling-0 --grace-period=0 --force >/dev/null 2>&1
LOST_AT=$(date -u +%H:%M:%S)
sleep 80
check "vBilling did not run meanwhile" "[ \"\$(kubectl -n vbilling get pod vbilling-0 -o jsonpath='{.status.phase}' 2>/dev/null)\" != Running ]"
kubectl uncordon "$NODE" >/dev/null
check "a new pod starts on a new, empty data volume" "wait_for 240 '[ -n \"\$(uid pvc/data-vbilling-0)\" ] && [ \"\$(uid pvc/data-vbilling-0)\" != \"$DATA_PVC\" ] && [ -n \"\$(uid pod/vbilling-0)\" ] && [ \"\$(uid pod/vbilling-0)\" != \"$POD\" ]'"
check "the archive volume is the one from before" "[ \"\$(uid pvc/archive-vbilling-0)\" = \"$ARCHIVE_PVC\" ]"
check "vBilling is ready again" reconnect
echo "  volume deleted at $LOST_AT, ready again at $(date -u +%H:%M:%S)"

step "vBilling restored itself from the mirror"
logs | grep '\[mirror\]' | cut -c1-260 | sed 's/^/  /' || true
check "it found no ledger and restored the mirror's" "logged 'holds no ledger, but the mirror in .* does' && logged 'restored /var/lib/vbilling as it was at'"
check "the metering source identity is the same, so event IDs are" "[ \"\$(identity)\" = \"$ID\" ]"
check "the ledger verifies" "[ \"\$(api /api/v1/ledger/verify | jq .ok)\" = true ]"
RESTORED_TO=$(logs | sed -n 's/.*restored \/var\/lib\/vbilling\/ledger: seq [0-9]*-\([0-9]*\),.*/\1/p' | tail -1)
check "the restored ledger holds what the mirror had (to seq ${RESTORED_TO:-?}, the pushed usage is at $AT)" "[ -n \"$RESTORED_TO\" ] && [ \"${RESTORED_TO:-0}\" -ge \"$AT\" ]"
AFTER_END=$(minute 120)
check "the windows since the loss are metered" "wait_for 300 'window_done $AFTER_END'"
events >"$WORK/after.jsonl"
python3 -I - "$WORK/before.jsonl" "$WORK/after.jsonl" "$START" "$AFTER_END" >"$WORK/compare.json" <<'EOF'
import json, sys, datetime
p = lambda s: datetime.datetime.fromisoformat(s.replace("Z", "+00:00"))
def load(path):
    return [json.loads(l) for l in open(path) if l.startswith("{")]
before, after = load(sys.argv[1]), load(sys.argv[2])
start, end = p(sys.argv[3]), p(sys.argv[4])
ids = [e["id"] for e in after]
have = set(ids)
# What was recorded before the loss, the last-second push aside.
missing = sorted(e["id"] for e in before if e["id"] not in have and e["id"] != "job-last-second")
# Every minute between the first metered window and the last has CPU hours.
cpu = sorted({p(e["window_start"]) for e in after if e["metric"] == "vcluster_cpu_core_hours" and start <= p(e["window_start"]) < end})
gaps = []
if cpu:
    t = cpu[0]
    while t < cpu[-1]:
        if t not in cpu: gaps.append(t.strftime("%H:%M"))
        t += datetime.timedelta(minutes=1)
changed = sorted(e["id"] for e in before for a in after if a["id"] == e["id"] and e["metric"] == "batch_jobs" and a["quantity"] != e["quantity"])
print(json.dumps({"before": len(before), "after": len(after), "missing": missing[:5], "missing_count": len(missing),
                  "duplicates": len(ids) - len(have), "cpu_windows": len(cpu), "gaps": gaps,
                  "backfilled": sum(1 for e in after if (e.get("properties") or {}).get("backfilled")),
                  "early_push": sum(e["quantity"] for e in after if e["id"] == "job-early"), "changed": changed}))
EOF
cat "$WORK/compare.json"
c() { jq -r "$1" "$WORK/compare.json"; }
check "every event recorded before the loss is there, under the same ID" "[ \"\$(c .missing_count)\" = 0 ] && [ \"\$(c .before)\" -gt 0 ]"
check "no event ID repeats" "[ \"\$(c .duplicates)\" = 0 ]"
check "no window is missing across the loss" "[ \"\$(c '.gaps | length')\" = 0 ] && [ \"\$(c .cpu_windows)\" -ge 5 ]"
check "the windows of the outage are marked as metered late" "[ \"\$(c .backfilled)\" -ge 1 ]"
check "the usage pushed before the loss is there, unchanged" "[ \"\$(c .early_push)\" = 7 ] && [ \"\$(c '.changed | length')\" = 0 ]"
check "delivery continues from its cursor" "wait_for 60 'api /api/v1/destinations | jq -e \".destinations[] | select(.name==\\\"noop\\\") | .state==\\\"delivering\\\" and .lag_records==0\" >/dev/null'"
check "the mirror is in step again" "wait_for 60 in_step"

step "A sender that retries fills in what was pushed in the last second"
SURVIVED=$(grep -c '"id":"job-last-second"' "$WORK/after.jsonl" || true)
echo "  pushed in the last second before the loss: $([ "$SURVIVED" = 1 ] && echo 'in the restored ledger' || echo 'not in the restored ledger')"
push job-last-second 3 "$PUSH_WINDOW" >"$WORK/retry.json" || true
cat "$WORK/retry.json"; echo
events >"$WORK/final.jsonl"
check "the retried push is counted exactly once" "[ \"\$(grep -c '\"id\":\"job-last-second\"' '$WORK/final.jsonl')\" = 1 ]"

step "Moving the ledger to another storage class"
# The documented procedure (configuration.html#move-the-ledger), inside this
# test's own kind cluster: a clean stop, which ends with a last mirror pass,
# the StatefulSet and the old claim removed, and an upgrade with the new
# class. Usage pushed right before the stop has to come back with the rest.
kubectl apply -f - >/dev/null <<'EOF'
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata: {name: e2e-other}
provisioner: rancher.io/local-path
volumeBindingMode: WaitForFirstConsumer
reclaimPolicy: Delete
EOF
MOVED_FROM=$(uid pvc/data-vbilling-0)
push job-before-move 5 "$PUSH_WINDOW" >"$WORK/before-move-push.json" || true
events >"$WORK/before-move.jsonl"
kubectl -n vbilling scale statefulset vbilling --replicas=0 >/dev/null
kubectl -n vbilling wait --for=delete pod/vbilling-0 --timeout=120s >/dev/null
kubectl -n vbilling delete statefulset vbilling >/dev/null
kubectl -n vbilling delete pvc data-vbilling-0 >/dev/null
helm upgrade vbilling "$ROOT/deploy/helm/vbilling" -n vbilling -f "$WORK/values.yaml" --set persistence.storageClass=e2e-other --wait --timeout 240s >/dev/null
check "vBilling is ready on the new volume" reconnect
check "the data volume is new and of the new class" "[ \"\$(kubectl -n vbilling get pvc data-vbilling-0 -o jsonpath='{.spec.storageClassName}')\" = e2e-other ] && [ \"\$(uid pvc/data-vbilling-0)\" != \"$MOVED_FROM\" ]"
check "the archive volume is still the one from the start" "[ \"\$(uid pvc/archive-vbilling-0)\" = \"$ARCHIVE_PVC\" ]"
check "it restored itself from the mirror" "logged 'holds no ledger, but the mirror in .* does' && logged 'restored /var/lib/vbilling as it was at'"
check "the metering source identity is still the same" "[ \"\$(identity)\" = \"$ID\" ]"
check "the ledger verifies after the move" "[ \"\$(api /api/v1/ledger/verify | jq .ok)\" = true ]"
events >"$WORK/after-move.jsonl"
python3 -I - "$WORK/before-move.jsonl" "$WORK/after-move.jsonl" >"$WORK/move.json" <<'EOF'
import json, sys
def load(path):
    return [json.loads(l) for l in open(path) if l.startswith("{")]
before, after = load(sys.argv[1]), load(sys.argv[2])
ids = [e["id"] for e in after]
have = set(ids)
print(json.dumps({"before": len(before), "after": len(after), "missing": sorted(e["id"] for e in before if e["id"] not in have)[:5],
                  "missing_count": sum(1 for e in before if e["id"] not in have), "duplicates": len(ids) - len(have),
                  "pushed_before_the_stop": sum(e["quantity"] for e in after if e["id"] == "job-before-move")}))
EOF
cat "$WORK/move.json"
moved() { jq -r "$1" "$WORK/move.json"; }
check "every event from before the move is there, none twice" "[ \"\$(moved .missing_count)\" = 0 ] && [ \"\$(moved .duplicates)\" = 0 ] && [ \"\$(moved .before)\" -gt 0 ]"
check "the usage pushed right before the stop came with it" "[ \"\$(moved .pushed_before_the_stop)\" = 5 ]"
check "the mirror follows the ledger on its new volume" "wait_for 60 in_step"

step "Without the mirror, source health says that no second copy is kept"
helm upgrade vbilling "$ROOT/deploy/helm/vbilling" -n vbilling -f "$WORK/values.yaml" --set persistence.storageClass=e2e-other --set persistence.archive.mirror=off --wait --timeout 240s >/dev/null
kubectl -n vbilling rollout status sts/vbilling --timeout=240s >/dev/null
check "vBilling is ready with the mirror off" reconnect
check "the source is paused and says why" "wait_for 60 'mirror | jq -e \".status==\\\"paused\\\" and (.message | contains(\\\"LEDGER_MIRROR=off\\\"))\" >/dev/null'"
check "the log names the copy that is no longer updated" "logged 'holds a copy from when the mirror was kept'"
check "no storage source counts as failing" "[ \"\$(api /api/v1/sources | jq '[.sources[] | select(.kind==\"storage\" and .status==\"failing\")] | length')\" = 0 ]"

echo
echo "$pass passed, $fail failed"
[ "$fail" = 0 ]
