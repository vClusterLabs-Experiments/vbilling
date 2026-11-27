#!/usr/bin/env bash
# End-to-end test for pausing a tenant, on a laptop.
#
# Builds a kind control plane cluster with one vCluster, runs a workload with
# a volume inside it, installs vBilling from this repository's Helm chart in
# enforce mode, then:
#   - makes the tenant delinquent through the API and lets the escalation
#     rules (ENFORCEMENT_ESCALATIONS) suspend it after a minute and pause it a
#     minute later, with no further signal; checks the next escalation is
#     shown, the control plane is stopped and the synced workloads are gone;
#   - checks the cluster cannot be reached while paused, and that connecting
#     with `vcluster connect` does not resume it;
#   - checks billing: no compute or control plane hours for paused time (to
#     the second), while the volume keeps billing (PAUSE_KEEP_BILLING default);
#   - scales the control plane back up by hand and checks vBilling pauses it
#     again within seconds: the pause is durable;
#   - clears the hold and checks the cluster and its workload come back.
#
# Usage: scripts/e2e-pause.sh [--keep]   (--keep leaves everything running)
# Requires: docker, kind, kubectl, helm, vcluster (0.30+), jq, python3, curl.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
WORK=${WORK:-$(mktemp -d -t vbilling-pause)}
CLUSTER=vbilling-e2e-pause
KEEP=${1:-}
API=127.0.0.1:28191
TOKEN=e2e-admin-token # the API always needs a token
export KUBECONFIG=$WORK/kubeconfig # never touches ~/.kube/config

pass=0; fail=0
ok()    { echo "  PASS  $*"; pass=$((pass+1)); }
bad()   { echo "  FAIL  $*"; fail=$((fail+1)); }
step()  { echo; echo "==> $*"; }
check() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }
api()   { curl -fsS -H "Authorization: Bearer $TOKEN" "http://$API$1"; }
put()   { curl -fsS -X PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d "$2" "http://$API$1"; }
vc()    { vcluster connect team-a -n vc-team-a --driver helm -- "$@"; }
utc()   { python3 -I -c 'import datetime,sys; print((datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(seconds=int(sys.argv[1]))).strftime("%Y-%m-%dT%H:%M:%SZ"))' "${1:-0}"; }
minute(){ python3 -I -c 'import datetime,sys; t=datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(seconds=int(sys.argv[1])); print(t.replace(second=0,microsecond=0).strftime("%Y-%m-%dT%H:%M:%SZ"))' "${1:-0}"; }
wait_for() { local end=$((SECONDS+$1)); until eval "$2"; do [ $SECONDS -lt $end ] || return 1; sleep 3; done; }
ready()   { api /api/v1/status 2>/dev/null | jq -e '.ready' >/dev/null; }
replicas(){ kubectl -n vc-team-a get sts team-a -o jsonpath='{.spec.replicas}'; }
ann()     { kubectl -n vc-team-a get sts team-a -o json | jq -r --arg k "$1" '.metadata.annotations[$k] // ""'; }
synced()  { kubectl -n vc-team-a get pods -l vcluster.loft.sh/managed-by=team-a --no-headers 2>/dev/null | grep -c . || true; }
running() { kubectl -n vc-team-a get pods -l vcluster.loft.sh/managed-by=team-a --no-headers 2>/dev/null | grep -c Running || true; }
phase()   { api /api/v1/billing-states | jq -r '.pauses[] | select(.cluster=="vcluster-vc-team-a-team-a") | .phase'; }
window_done() { api /api/v1/status 2>/dev/null | jq -e --arg e "$1" '.ledger.watermarks.collector >= $e' >/dev/null; }

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
  kubectl -n vbilling port-forward svc/vbilling 28191:8080 >"$WORK/pf.log" 2>&1 & PIDS+=($!)
  wait_for 30 "curl -s -o /dev/null http://$API/" || true
}

step "Cluster, vCluster, workload"
kind create cluster --name "$CLUSTER" --wait 120s >/dev/null
kind get kubeconfig --name "$CLUSTER" >"$KUBECONFIG"
docker build -q -t vbilling:e2e-pause "$ROOT" >/dev/null
kind load docker-image vbilling:e2e-pause --name "$CLUSTER" >/dev/null
vcluster create team-a -n vc-team-a --driver helm --connect=false >/dev/null
kubectl label ns vc-team-a vbilling.vcluster.com/tenant=acme --overwrite >/dev/null
cat >"$WORK/workload.yaml" <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: checkpoints}
spec: {accessModes: [ReadWriteOnce], resources: {requests: {storage: 1Gi}}}
---
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
          command: [sh, -c, "while true; do date >> /ckpt/log; sleep 5; done"]
          resources: {requests: {cpu: 500m, memory: 128Mi}}
          volumeMounts: [{name: ckpt, mountPath: /ckpt}]
      volumes: [{name: ckpt, persistentVolumeClaim: {claimName: checkpoints}}]
EOF
vc kubectl apply -f "$WORK/workload.yaml" >/dev/null
check "the workload runs (synced to the control plane cluster)" "wait_for 180 '[ \"\$(running)\" -ge 1 ]'"

cat >"$WORK/values.yaml" <<EOF
image: {repository: vbilling, tag: e2e-pause, pullPolicy: Never}
adapters: [noop]
region: e2e
metering: {cpuMemoryBasis: requests}
enforcement:
  mode: enforce
  admissionPolicy: {enabled: true}
  escalations:
    - {from: delinquent, after: 1m, to: suspended}
    - {from: suspended, after: 1m, to: paused}
persistence: {enabled: true, size: 1Gi}
api: {token: $TOKEN}
EOF
helm install vbilling "$ROOT/deploy/helm/vbilling" -n vbilling --create-namespace -f "$WORK/values.yaml" --wait --timeout 180s >/dev/null
forward
check "vBilling is ready" "wait_for 120 ready"
BEFORE=$(minute 60)
check "a full window is metered before the pause" "wait_for 150 'window_done $(minute 120)'"

step "Grace period, then pause"
# The tenant's balance reaches zero: delinquent. The escalation rules raise
# the billing hold to suspended after a minute, and to paused a minute after
# that, with no further signal.
bstate() { api /api/v1/billing-states | jq -r ".states[] | select(.tenant==\"acme\") | $1"; }
secs() { python3 -I -c 'import sys,datetime,re; p=lambda s: datetime.datetime.fromisoformat(re.sub(r"(\.\d{6})\d+", r"\1", s).replace("Z","+00:00")); print(round((p(sys.argv[2])-p(sys.argv[1])).total_seconds()))' "$1" "$2"; }
put /api/v1/billing-states/acme '{"state":"delinquent","hold":"billing","reason":"prepaid balance reached zero"}' >/dev/null
check "a delinquent tenant shows its next escalation: suspended, a minute after it became delinquent" \
  "[ \"\$(bstate .escalation.to)\" = suspended ] && [ \"\$(secs \"\$(bstate .since)\" \"\$(bstate .escalation.at)\")\" = 60 ]"
check "nothing is stopped while it is delinquent" "[ \"\$(replicas)\" = 1 ] && [ \"\$(running)\" -ge 1 ]"
check "after the grace it is suspended by the rule, with no new signal" \
  "wait_for 90 '[ \"\$(bstate .state)\" = suspended ]' && bstate .escalated_by | grep -q 'delinquent:1m:suspended'"
check "the suspended tenant shows its next escalation: paused" "[ \"\$(bstate .escalation.to)\" = paused ]"
check "a minute later it is paused, and the pause completes" "wait_for 150 '[ \"\$(phase)\" = paused ]'"
check "the escalation is counted" \
  "api /metrics | grep -q 'vbilling_enforcement_escalations_total{from=\"delinquent\",to=\"suspended\"} 1' && api /metrics | grep -q 'vbilling_enforcement_escalations_total{from=\"suspended\",to=\"paused\"} 1'"
PAUSED_AT=$(ann vbilling.vcluster.com/paused-at)
# Billing stops when vBilling has verified the stop: the pause span's start.
STOPPED_AT=$(api /api/v1/billing-states | jq -r '.pause_spans["vcluster-vc-team-a-team-a"][0].from')
echo "  paused at $PAUSED_AT, stop verified at $STOPPED_AT"
check "the control plane is scaled to zero" "[ \"\$(replicas)\" = 0 ]"
check "it carries vBilling's pause annotations, not vcluster pause's" "[ \"\$(ann vbilling.vcluster.com/paused-by)\" = vbilling ] && [ \"\$(ann vbilling.vcluster.com/paused-replicas)\" = 1 ] && [ -z \"\$(ann loft.sh/paused)\" ]"
check "the synced workloads are gone" "wait_for 60 '[ \"\$(synced)\" = 0 ]'"
endpoints() { kubectl -n vc-team-a get endpointslices -l kubernetes.io/service-name=team-a -o json | jq '[.items[].endpoints[]?] | length'; }
check "the cluster's API has no endpoints" "wait_for 60 '[ \"\$(endpoints)\" = 0 ]'"
connect_out=$(timeout 30 vcluster connect team-a -n vc-team-a --driver helm -- kubectl get pods 2>&1 || true)
echo "  vcluster connect: $(grep -o 'scaled down to zero replicas' <<<"$connect_out" || echo "$connect_out" | tail -1)"
check "vcluster connect refuses instead of resuming it" "grep -q 'scaled down' <<<\"\$connect_out\" && [ \"\$(replicas)\" = 0 ]"

step "Billing while paused"
FROM=$(minute 60)   # first window that starts after the pause
TO=$(minute 180)    # two full paused windows
check "two paused windows are metered" "wait_for 240 'window_done $TO'"
api "/api/v1/events?from=$BEFORE&to=$TO&limit=100000" >"$WORK/events.jsonl"
python3 -I - "$WORK/events.jsonl" "$STOPPED_AT" "$FROM" >"$WORK/paused.json" <<'EOF'
import json, sys, datetime
import re
# Python parses at most microseconds: trim RFC 3339 nanoseconds.
p = lambda s: datetime.datetime.fromisoformat(re.sub(r"(\.\d{6})\d+", r"\1", s).replace("Z", "+00:00"))
paused, first_paused = p(sys.argv[2]), p(sys.argv[3])
out = {"paused_windows": {}, "pause_window": {}, "before": {}}
for line in open(sys.argv[1]):
    if not line.startswith("{"): continue
    e = json.loads(line)
    if e.get("source") != "collector": continue
    ws, we = p(e["window_start"]), p(e["window_end"])
    key = "paused_windows" if ws >= first_paused else ("pause_window" if ws <= paused < we else "before")
    d = out[key]; d[e["metric"]] = d.get(e["metric"], 0) + e["quantity"]
    if key == "pause_window": out["pause_window_start"] = e["window_start"]
out["expected_cp_hours"] = round((paused - p(out.get("pause_window_start", sys.argv[2]))).total_seconds() / 3600, 9)
print(json.dumps(out))
EOF
cat "$WORK/paused.json"; echo
m() { jq -r "$1" "$WORK/paused.json"; }
check "CPU was billed before the pause" "[ \"\$(m '.before.vcluster_cpu_core_hours // 0')\" != 0 ]"
check "no CPU hours in fully paused windows" "[ \"\$(m '.paused_windows.vcluster_cpu_core_hours // 0')\" = 0 ]"
check "no control plane hours in fully paused windows" "[ \"\$(m '.paused_windows.vcluster_instance_hours // 0')\" = 0 ]"
# The control plane bills while a replica is ready: it stops when its last
# replica is gone, some seconds before vBilling has verified the stop.
check "control plane hours stop when its last replica is gone, no later than the verified stop" \
  "python3 -I -c 'import sys; a,b=float(sys.argv[1]),float(sys.argv[2]); sys.exit(not (0 < a <= b+1.5/3600 and b-a <= 60/3600))' \"\$(m '.pause_window.vcluster_instance_hours // 0')\" \"\$(m .expected_cp_hours)\""
check "the volume keeps billing (1 GiB x 2 windows)" \
  "python3 -I -c 'import sys; sys.exit(abs(float(sys.argv[1])-2/60)>1e-6)' \"\$(m '.paused_windows.vcluster_storage_gb_hours // 0')\""

step "Durable"
kubectl -n vc-team-a scale sts team-a --replicas=1 >/dev/null
SCALED=$SECONDS
check "vBilling pauses a control plane scaled back up by hand within 30s" "wait_for 30 '[ \"\$(replicas)\" = 0 ]'"
echo "  paused again after $((SECONDS-SCALED))s"

step "Resume"
put /api/v1/billing-states/acme '{"state":"active","hold":"billing","reason":"top-up paid"}' >/dev/null
check "the control plane is back" "wait_for 60 '[ \"\$(replicas)\" = 1 ] && [ -z \"\$(ann vbilling.vcluster.com/paused-by)\" ]'"
check "the workload runs again" "wait_for 240 '[ \"\$(running)\" -ge 1 ]'"
check "the tenant reaches its cluster again" "wait_for 120 'vc kubectl get deploy trainer >/dev/null 2>&1'"
check "the paid tenant has no escalation pending" "[ -z \"\$(bstate '.escalation // empty')\" ] && [ \"\$(bstate .state)\" = active ]"
check "pause spans are recorded and closed" \
  "api /api/v1/billing-states | jq -e '[.pause_spans[\"vcluster-vc-team-a-team-a\"][] | select(.to == null)] | length == 0' >/dev/null"
api /api/v1/billing-states | jq -c '.pause_spans'

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
