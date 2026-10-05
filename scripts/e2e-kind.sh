#!/usr/bin/env bash
# End-to-end test for vBilling against real tenant clusters.
#
# Builds a kind control plane cluster with three workers advertising
# simulated GPUs (an H100 node, an L40S spot node, a dedicated bare-metal
# H100 node), creates three vCluster tenant clusters with GPU workloads, and
# runs vBilling against them with two destinations:
#   - stripe-mock (Stripe's official mock, validating every request against
#     Stripe's OpenAPI spec)
#   - a local receiver verifying vBilling's signed CloudEvents webhooks
# It then asserts metered quantities, delivery, ledger integrity, and the
# billing-state enforcement loop (signed Stripe webhook -> admission policy
# blocks new pods -> payment lifts the block).
#
# Usage: scripts/e2e-kind.sh [--keep]   (--keep leaves everything running)
# Requires: docker, kind, kubectl, helm, vcluster (0.30+), go, jq, python3.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
WORK=${WORK:-$(mktemp -d -t vbilling-e2e)}
CLUSTER=vbilling-e2e
KEEP=${1:-}
API=127.0.0.1:18080
TOKEN=e2e-api
STRIPE_WHSEC=whsec_e2e
WEBHOOK_SECRET=e2e-webhook-secret
export KUBECONFIG=$WORK/kubeconfig # never touches ~/.kube/config

pass=0; fail=0
ok()   { echo "  PASS  $*"; pass=$((pass+1)); }
bad()  { echo "  FAIL  $*"; fail=$((fail+1)); }
step() { echo; echo "==> $*"; }
check() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }
api()  { curl -fsS -H "Authorization: Bearer $TOKEN" "http://$API$1"; }

PIDS=()
cleanup() {
  if [ "$KEEP" != "--keep" ]; then
    for p in "${PIDS[@]:-}"; do kill "$p" 2>/dev/null || true; done
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
    docker rm -f vb-stripe-mock >/dev/null 2>&1 || true
  else
    echo "Left running (vBilling, receiver, cluster). KUBECONFIG=$KUBECONFIG, API http://$API (token $TOKEN), work dir $WORK"
  fi
}
trap cleanup EXIT

step "kind control plane cluster with simulated GPU nodes"
cat > "$WORK/kind.yaml" <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
    labels: {nvidia.com/gpu.product: NVIDIA-H100-80GB-HBM3, topology.kubernetes.io/zone: syd-1a}
  - role: worker
    labels: {nvidia.com/gpu.product: NVIDIA-L40S, vbilling.vcluster.com/capacity-type: spot, topology.kubernetes.io/zone: syd-1b}
  - role: worker
    labels: {nvidia.com/gpu.product: NVIDIA-H100-80GB-HBM3, node.kubernetes.io/instance-type: bm.gpu.h100.8, vcluster.loft.sh/managed-by: team-b, topology.kubernetes.io/zone: syd-1a}
EOF
kind create cluster --name "$CLUSTER" --config "$WORK/kind.yaml" --kubeconfig "$KUBECONFIG" --wait 120s >/dev/null 2>&1
for n in $CLUSTER-worker $CLUSTER-worker2 $CLUSTER-worker3; do
  kubectl patch node "$n" --subresource=status --type=json \
    -p '[{"op":"add","path":"/status/capacity/nvidia.com~1gpu","value":"8"},{"op":"add","path":"/status/allocatable/nvidia.com~1gpu","value":"8"}]' >/dev/null
done
kubectl taint node $CLUSTER-worker3 dedicated=team-b:NoSchedule >/dev/null
kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml >/dev/null
kubectl -n kube-system patch deployment metrics-server --type=json \
  -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]' >/dev/null

step "three tenant clusters"
for t in team-a team-b team-c; do vcluster create "$t" -n "$t" --driver helm --connect=false >/dev/null 2>&1 & done
wait
for t in team-a team-b team-c; do kubectl wait --for=jsonpath='{.status.readyReplicas}'=1 "sts/$t" -n "$t" --timeout=300s >/dev/null; done
P=vbilling.vcluster.com
kubectl annotate ns team-a $P/tenant=acme "$P/display-name=Acme AI" $P/tenant-class=public $P/project=training --overwrite >/dev/null
kubectl annotate ns team-c $P/tenant=acme $P/project=inference --overwrite >/dev/null
kubectl annotate ns team-b "$P/display-name=Globex Research" $P/tenant-class=enterprise --overwrite >/dev/null

gpu_deploy() { # name ns replicas gpus nodeSelector [toleration]
  cat <<EOF
apiVersion: apps/v1
kind: Deployment
metadata: {name: $1, namespace: $2}
spec:
  replicas: $3
  selector: {matchLabels: {app: $1}}
  template:
    metadata: {labels: {app: $1}}
    spec:
      nodeSelector: {$5}
      tolerations: [${6:-}]
      containers:
        - name: main
          image: busybox:1.36
          command: ["sh", "-c", "while true; do sleep 3600; done"]
          resources: {requests: {cpu: 100m, memory: 64Mi}, limits: {nvidia.com/gpu: $4}}
EOF
}
{ echo "apiVersion: v1"; echo "kind: Namespace"; echo "metadata: {name: training}"; echo "---"; gpu_deploy trainer training 2 2 "nvidia.com/gpu.product: NVIDIA-H100-80GB-HBM3, topology.kubernetes.io/zone: syd-1a"; } > "$WORK/team-a.yaml"
{ echo "apiVersion: v1"; echo "kind: Namespace"; echo "metadata: {name: inference}"; echo "---"; gpu_deploy llm inference 1 1 "nvidia.com/gpu.product: NVIDIA-L40S"; } > "$WORK/team-c.yaml"
gpu_deploy pretrain default 1 8 "vcluster.loft.sh/managed-by: team-b" "{key: dedicated, operator: Equal, value: team-b, effect: NoSchedule}" > "$WORK/team-b.yaml"
for t in team-a team-b team-c; do vcluster connect "$t" -n "$t" --driver helm -- kubectl apply -f "$WORK/$t.yaml" >/dev/null 2>&1; done
for t in team-a team-b team-c; do kubectl wait --for=condition=Ready pods -n "$t" -l vcluster.loft.sh/managed-by="$t" --timeout=180s >/dev/null 2>&1 || true; done

step "stripe-mock, webhook receiver, vBilling"
docker rm -f vb-stripe-mock >/dev/null 2>&1 || true
docker run -d --name vb-stripe-mock -p 12111:12111 stripe/stripe-mock:latest >/dev/null
cat > "$WORK/receiver.py" <<EOF
import hmac, hashlib, json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
class H(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers["Content-Length"]))
        sig = dict(p.split("=", 1) for p in self.headers.get("X-VBilling-Signature", "").split(","))
        ok = hmac.compare_digest(hmac.new(b"$WEBHOOK_SECRET", (sig.get("t", "") + ".").encode() + body, hashlib.sha256).hexdigest(), sig.get("v1", ""))
        with open("$WORK/received.jsonl", "a") as f:
            for ce in json.loads(body): f.write(json.dumps({"sig_ok": ok, "type": ce["type"], "id": ce["id"]}) + "\n")
        self.send_response(200 if ok else 401); self.end_headers()
    def log_message(self, *a): pass
HTTPServer(("127.0.0.1", 19999), H).serve_forever()
EOF
python3 "$WORK/receiver.py" & PIDS+=($!)
(cd "$ROOT" && CGO_ENABLED=0 go build -o "$WORK/vbilling" ./cmd/vbilling)
ADAPTERS=stripe,webhook STRIPE_API_KEY=sk_test_123 STRIPE_API_BASE=http://localhost:12111 STRIPE_WEBHOOK_SECRET=$STRIPE_WHSEC \
WEBHOOK_URL=http://127.0.0.1:19999/events WEBHOOK_SECRET=$WEBHOOK_SECRET \
REGION=ap-southeast-2 CLUSTER_NAME=syd-cp-1 COLLECTION_INTERVAL=20s RECONCILE_INTERVAL=10s \
DATA_DIR=$WORK/data LISTEN_ADDR=$API API_TOKEN=$TOKEN CPU_MEMORY_BASIS=requests ENFORCEMENT_MODE=enforce \
  "$WORK/vbilling" > "$WORK/vbilling.log" 2>&1 & PIDS+=($!)
# Wait until a few windows have closed after every workload was running, so
# the window checked below is fully covered (pods started mid-window are,
# correctly, billed only for the seconds they ran).
until [ "$(grep -c 'window .* events' "$WORK/vbilling.log" 2>/dev/null)" -ge 5 ]; do sleep 2; done
sleep 5

step "metering"
WIN=$(api "/api/v1/events?metric=vcluster_gpu_hours" | jq -s -r '[.[] | .window_start] | unique | .[-1]')
WEND=$(date -u -j -v+20S -f "%Y-%m-%dT%H:%M:%SZ" "$WIN" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u -d "$WIN + 20 seconds" +%Y-%m-%dT%H:%M:%SZ)
q() { api "/api/v1/usage?from=$WIN&to=$WEND&group_by=tenant,sku,billing_mode&metric=vcluster_gpu_hours" | jq -r "[.rows[] | select($1) | .quantity] | add // 0"; }
check "acme H100: 2 pods x 2 GPUs x 20s = 0.022222222 GPU-hours" '[ "$(q '"'"'.tenant=="acme" and .sku=="NVIDIA-H100-80GB-HBM3"'"'"')" = "0.022222222" ]'
check "acme L40S (spot): 1 GPU x 20s = 0.005555556" '[ "$(q '"'"'.tenant=="acme" and .sku=="NVIDIA-L40S"'"'"')" = "0.005555556" ]'
check "Globex dedicated node: 8 GPUs x 20s = 0.044444444, billing_mode=dedicated_node" '[ "$(q '"'"'.tenant=="vcluster-team-b-team-b" and .billing_mode=="dedicated_node"'"'"')" = "0.044444444" ]'
check "Globex pod on its own dedicated node is not billed again as shared" '[ "$(q '"'"'.tenant=="vcluster-team-b-team-b" and .billing_mode=="shared"'"'"')" = "0" ]'
check "two tenant clusters roll up into one customer" '[ "$(api /api/v1/tenants | jq "[.tenants[] | select(.id==\"acme\") | .clusters | length] | .[0]")" = "2" ]'

step "delivery and integrity"
sleep 5
check "stripe and webhook delivering with no lag or dead letters" '[ "$(api /api/v1/destinations | jq "[.destinations[] | select(.ready and .lag_records==0 and .dead_letters==0)] | length")" = "2" ]'
check "ledger hash chain verifies" '[ "$(api /api/v1/ledger/verify | jq .ok)" = "true" ]'
check "every webhook signature valid" '[ "$(jq -s "all(.sig_ok)" "$WORK/received.jsonl")" = "true" ]'
check "no duplicate event IDs delivered" '[ "$(jq -s "[.[] | select(.type|endswith(\"usage.v1\")) | .id] | (length == (unique|length))" "$WORK/received.jsonl")" = "true" ]'

step "billing-state enforcement"
(cd "$ROOT" && helm template vbilling deploy/helm/vbilling --set enforcement.admissionPolicy.enabled=true --show-only templates/admissionpolicy.yaml) | kubectl apply -f - >/dev/null
CUS=$(jq -r '.["cus/acme"]' "$WORK/data/state-stripe.json")
send() { python3 - "$1" "$2" "$3" "$CUS" <<'EOF'
import hmac, hashlib, json, sys, time, urllib.request
typ, status, evid, cus = sys.argv[1:5]
body = json.dumps({"id": evid, "type": typ, "data": {"object": {"customer": cus, "status": status}}}).encode()
t = str(int(time.time())); sig = hmac.new(b"whsec_e2e", (t + ".").encode() + body, hashlib.sha256).hexdigest()
urllib.request.urlopen(urllib.request.Request("http://127.0.0.1:18080/webhooks/stripe", data=body, headers={"Stripe-Signature": f"t={t},v1={sig}"}))
EOF
}
send customer.subscription.updated unpaid evt_e2e_1
check "unpaid subscription suspends both of acme's namespaces" '[ "$(kubectl get ns team-a -o jsonpath="{.metadata.labels.vbilling\.vcluster\.com/suspended}")" = "true" ] && [ "$(kubectl get ns team-c -o jsonpath="{.metadata.labels.vbilling\.vcluster\.com/suspended}")" = "true" ]'
check "other tenants are untouched" '[ -z "$(kubectl get ns team-b -o jsonpath="{.metadata.labels.vbilling\.vcluster\.com/suspended}")" ]'
vcluster connect team-a -n team-a --driver helm -- kubectl run probe -n training --image=busybox:1.36 --restart=Never -- sleep 600 >/dev/null 2>&1
sleep 10
check "admission policy blocks new pods for the suspended tenant" '[ "$(kubectl get pods -n team-a --no-headers 2>/dev/null | grep -c "^probe-")" = "0" ]'
check "running workloads keep running" '[ "$(kubectl get pods -n team-a --no-headers | grep trainer | grep -c Running)" = "2" ]'
send invoice.paid "" evt_e2e_2
sleep 20
check "a paid invoice lifts the suspension and the pod starts" '[ "$(kubectl get pods -n team-a --no-headers | grep "^probe-" | grep -c Running)" = "1" ]'

echo
echo "$pass passed, $fail failed  (logs: $WORK/vbilling.log)"
[ "$fail" -eq 0 ]
