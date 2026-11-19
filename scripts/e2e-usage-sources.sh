#!/usr/bin/env bash
# End-to-end test for vBilling's usage sources, on a laptop.
#
# Builds a kind control plane cluster with one vCluster tenant cluster, a
# Prometheus that scrapes it, and an "engine" pod inside the tenant cluster
# that exposes vLLM's output token counter (+100 every 5 seconds). vBilling
# is installed from this repository's Helm chart, then the script:
#   - posts LiteLLM-format request records (with retried batches and one
#     late record) and checks the ledger bills exactly what was sent;
#   - checks the counter metric bills exactly the increase Prometheus stored,
#     across a vBilling outage that spans window boundaries, while the
#     increase() estimate of the same counter is marked as an estimate;
#   - checks the cross-check and source health APIs;
#   - suspends the tenant and checks a pod is refused inside its own cluster,
#     that a payment does not lift an operator's hold, and that lifting it
#     allows pods again.
#
# Usage: scripts/e2e-usage-sources.sh [--keep]   (--keep leaves everything running)
# Requires: docker, kind, kubectl, helm, vcluster (0.30+), jq, python3, curl.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
WORK=${WORK:-$(mktemp -d -t vbilling-sources)}
CLUSTER=vbilling-e2e-sources
KEEP=${1:-}
API=127.0.0.1:28181
PROM=127.0.0.1:29191
INGEST=e2e-ingest
TOKEN=e2e-admin-token # the API always needs a token
export KUBECONFIG=$WORK/kubeconfig # never touches ~/.kube/config

pass=0; fail=0
ok()    { echo "  PASS  $*"; pass=$((pass+1)); }
bad()   { echo "  FAIL  $*"; fail=$((fail+1)); }
step()  { echo; echo "==> $*"; }
check() { if eval "$2"; then ok "$1"; else bad "$1"; fi; }
api()   { curl -fsS -H "Authorization: Bearer $TOKEN" "http://$API$1"; }
vc()    { vcluster connect team-a -n vc-team-a --driver helm -- "$@"; }
utc()   { python3 -I -c 'import datetime,sys; print((datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(seconds=int(sys.argv[1]))).strftime("%Y-%m-%dT%H:%M:%SZ"))' "${1:-0}"; }
minute(){ python3 -I -c 'import datetime,sys; t=datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(seconds=int(sys.argv[1])); print(t.replace(second=0,microsecond=0).strftime("%Y-%m-%dT%H:%M:%SZ"))' "${1:-0}"; }
# wait_for SECONDS CONDITION: poll a condition instead of sleeping blindly.
wait_for() { local end=$((SECONDS+$1)); until eval "$2"; do [ $SECONDS -lt $end ] || return 1; sleep 3; done; }
after()  { [[ "$(utc)" > "$1" ]]; }
ready()  { api /api/v1/status 2>/dev/null | jq -e '.ready' >/dev/null; }
scraped(){ curl -fsS "http://$PROM/api/v1/query?query=vllm:generation_tokens_total" 2>/dev/null | jq -e '.data.result | length > 0' >/dev/null; }
no_pods(){ [ -z "$(kubectl -n vbilling get pods --no-headers 2>/dev/null)" ]; }
caught_up() { api /api/v1/status 2>/dev/null | jq -e --arg e "$END" '[.ledger.watermarks[]] | all(. >= $e)' >/dev/null; }
total()  { jq -r ".$1" "$WORK/totals.json"; }
# A new pod name on every attempt: an admission policy takes a moment to
# take effect, and a pod created before it would make every retry fail with
# "already exists" instead of the denial.
# kubectl exits 1 when a pod is denied, so capture first: under pipefail a
# pipe into grep would fail even when grep finds the denial.
refused(){ local out; out=$(vc kubectl run "$1-$RANDOM" --image=busybox --restart=Never -- sleep 5 2>&1) || true; grep -q "$2" <<<"$out"; }
allowed(){ vc kubectl run "$1-$RANDOM" --image=busybox --restart=Never -- sleep 5 >/dev/null 2>&1; }
plus()   { python3 -I -c 'import datetime,sys; t=datetime.datetime.strptime(sys.argv[1],"%Y-%m-%dT%H:%M:%SZ")+datetime.timedelta(seconds=int(sys.argv[2])); print(t.strftime("%Y-%m-%dT%H:%M:%SZ"))' "$1" "$2"; }

PIDS=()
cleanup() {
  for p in "${PIDS[@]:-}"; do kill "$p" 2>/dev/null || true; done
  if [ "$KEEP" != "--keep" ]; then
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
    rm -rf "$WORK"
  else
    echo "Kept: KUBECONFIG=$KUBECONFIG, API http://$API, Prometheus http://$PROM"
  fi
}
trap cleanup EXIT
forward() { # forward NS SVC LOCAL REMOTE
  kubectl -n "$1" port-forward "svc/$2" "$3:$4" >"$WORK/pf-$2.log" 2>&1 & PIDS+=($!)
  wait_for 30 "curl -s -o /dev/null http://127.0.0.1:$3/" || true
}

step "Cluster, vCluster, Prometheus"
kind create cluster --name "$CLUSTER" --wait 120s >/dev/null
kind get kubeconfig --name "$CLUSTER" >"$KUBECONFIG"
docker build -q -t vbilling:e2e-sources "$ROOT" >/dev/null
kind load docker-image vbilling:e2e-sources --name "$CLUSTER" >/dev/null
# vBilling reaches the cluster with the read-only kubeconfig vCluster exports,
# plus the documented right to manage its own admission policies.
cat >"$WORK/vcluster.yaml" <<'VC'
exportKubeConfig:
  additionalSecrets:
    - name: vbilling-kubeconfig
      serviceAccount:
        name: vbilling-reader
        namespace: kube-system
        clusterRole: vbilling-reader
experimental:
  deploy:
    vcluster:
      manifests: |-
        apiVersion: rbac.authorization.k8s.io/v1
        kind: ClusterRole
        metadata:
          name: vbilling-reader
        rules:
          - apiGroups: [""]
            resources: [nodes, pods, persistentvolumeclaims, services, namespaces]
            verbs: [get, list, watch]
          - apiGroups: [coordination.k8s.io]
            resources: [leases]
            verbs: [get]
        ---
        apiVersion: rbac.authorization.k8s.io/v1
        kind: ClusterRole
        metadata:
          name: vbilling-enforcer
        rules:
          - apiGroups: [admissionregistration.k8s.io]
            resources: [validatingadmissionpolicies, validatingadmissionpolicybindings]
            verbs: [create]
          - apiGroups: [admissionregistration.k8s.io]
            resources: [validatingadmissionpolicies, validatingadmissionpolicybindings]
            resourceNames: [vbilling-suspended, vbilling-paused]
            verbs: [get, update, delete]
        ---
        apiVersion: rbac.authorization.k8s.io/v1
        kind: ClusterRoleBinding
        metadata:
          name: vbilling-enforcer
        roleRef:
          apiGroup: rbac.authorization.k8s.io
          kind: ClusterRole
          name: vbilling-enforcer
        subjects:
          - kind: ServiceAccount
            name: vbilling-reader
            namespace: kube-system
VC
vcluster create team-a -n vc-team-a --driver helm --connect=false -f "$WORK/vcluster.yaml" >/dev/null
kubectl label ns vc-team-a vbilling.vcluster.com/tenant=acme --overwrite >/dev/null
kubectl annotate ns vc-team-a vbilling.vcluster.com/aliases=team-acme-gw --overwrite >/dev/null

cat >"$WORK/prometheus.yaml" <<'EOF'
apiVersion: v1
kind: Namespace
metadata: {name: monitoring}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: prometheus, namespace: monitoring}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: e2e-prometheus}
rules: [{apiGroups: [""], resources: [pods], verbs: [get, list, watch]}]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: e2e-prometheus}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: e2e-prometheus}
subjects: [{kind: ServiceAccount, name: prometheus, namespace: monitoring}]
---
apiVersion: v1
kind: ConfigMap
metadata: {name: prometheus, namespace: monitoring}
data:
  prometheus.yml: |
    global: {scrape_interval: 10s}
    scrape_configs:
      - job_name: pods
        kubernetes_sd_configs: [{role: pod}]
        relabel_configs:
          - {source_labels: [__meta_kubernetes_pod_annotation_prometheus_io_scrape], regex: "true", action: keep}
          - {source_labels: [__address__, __meta_kubernetes_pod_annotation_prometheus_io_port], regex: '([^:]+)(?::\d+)?;(\d+)', replacement: '$1:$2', target_label: __address__}
          - {source_labels: [__meta_kubernetes_namespace], target_label: namespace}
          - {source_labels: [__meta_kubernetes_pod_name], target_label: pod}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: prometheus, namespace: monitoring}
spec:
  selector: {matchLabels: {app: prometheus}}
  template:
    metadata: {labels: {app: prometheus}}
    spec:
      serviceAccountName: prometheus
      containers:
        - name: prometheus
          image: prom/prometheus:v2.55.1
          args: [--config.file=/etc/prometheus/prometheus.yml, --storage.tsdb.retention.time=1d]
          volumeMounts: [{name: config, mountPath: /etc/prometheus}]
      volumes: [{name: config, configMap: {name: prometheus}}]
---
apiVersion: v1
kind: Service
metadata: {name: prometheus, namespace: monitoring}
spec: {selector: {app: prometheus}, ports: [{port: 9090}]}
EOF
kubectl apply -f "$WORK/prometheus.yaml" >/dev/null

cat >"$WORK/engine.yaml" <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata: {name: engine, namespace: default}
spec:
  selector: {matchLabels: {app: engine}}
  template:
    metadata:
      labels: {app: engine}
      annotations: {prometheus.io/scrape: "true", prometheus.io/port: "8000"}
    spec:
      terminationGracePeriodSeconds: 1
      containers:
        - name: engine
          image: python:3.12-alpine
          command: [python, -c]
          args:
            - |
              import http.server, time
              start = time.time()
              class H(http.server.BaseHTTPRequestHandler):
                  def do_GET(self):
                      v = int((time.time() - start) // 5) * 100
                      body = ('vllm:generation_tokens_total{model_name="llama-70b"} %d\n' % v).encode()
                      self.send_response(200); self.end_headers(); self.wfile.write(body)
                  def log_message(self, *a): pass
              http.server.HTTPServer(('', 8000), H).serve_forever()
EOF
vc kubectl apply -f "$WORK/engine.yaml" >/dev/null

cat >"$WORK/values.yaml" <<EOF
image: {repository: vbilling, tag: e2e-sources, pullPolicy: Never}
adapters: [noop]
region: e2e
api: {token: $TOKEN, ingestToken: $INGEST, tenantIngestTokens: {acme: gw-acme}}
prometheus:
  url: http://prometheus.monitoring.svc:9090
  gpuUtilQuery: none
  metrics:
    - {code: engine_output_tokens, unit: tokens, billable: false, skuLabel: model_name, counter: 'vllm:generation_tokens_total{{selector}}'}
    - {code: engine_output_tokens_est, unit: tokens, billable: false, query: 'sum(increase(vllm:generation_tokens_total{{selector}}[{{window}}]))'}
records: {litellm: true}
crossChecks: [{primary: inference_output_tokens, secondary: engine_output_tokens, tolerancePercent: 2}]
enforcement: {mode: enforce, inCluster: true, admissionPolicy: {enabled: true}}
persistence: {enabled: true, size: 1Gi}
EOF
helm install vbilling "$ROOT/deploy/helm/vbilling" -n vbilling --create-namespace -f "$WORK/values.yaml" --wait --timeout 180s >/dev/null
forward vbilling vbilling 28181 8080
forward monitoring prometheus 29191 9090
check "vBilling is ready" "wait_for 120 ready"
check "the engine counter is scraped" "wait_for 120 scraped"
START=$(minute 60) # first full window after the counter is visible

step "Gateway records (LiteLLM format) with retries and a late record"
cat >"$WORK/gateway.py" <<'EOF'
import json, sys, time, uuid, urllib.request
url, token, n = sys.argv[1], sys.argv[2], int(sys.argv[3])
prev, tokens = None, 0
for i in range(n):
    now = time.time()
    batch = [{"id": "chatcmpl-" + uuid.uuid4().hex[:12], "status": "success", "model_group": "llama-70b",
              "prompt_tokens": 50, "completion_tokens": 90, "endTime": now,
              "metadata": {"user_api_key_team_id": "team-acme-gw"}}]
    for body in [batch] + ([prev] if (i % 5 == 4 and prev) else []):  # every 5th post repeats the last batch
        urllib.request.urlopen(urllib.request.Request(url, data=json.dumps(body).encode(), method="POST",
                               headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"})).read()
    tokens += 90
    prev = batch
    time.sleep(5)
print(tokens)
EOF
wait_for 90 "after $START"
SENT=$(python3 -I "$WORK/gateway.py" "http://$API/api/v1/records?format=litellm" "$INGEST" 24)
LATE_AT=$(plus "$START" 10) # inside the first window, which closed two minutes ago
late_ok() {
  curl -fsS -X POST "http://$API/api/v1/records" -H 'Authorization: Bearer gw-acme' \
    -d "{\"id\":\"late-1\",\"time\":\"$LATE_AT\",\"usage\":{\"inference_output_tokens\":77}}" | jq -e '.accepted == 1' >/dev/null
}
check "a late record with a tenant-bound token is accepted" late_ok
SENT=$((SENT + 77))

step "A vBilling outage across window boundaries"
kubectl -n vbilling scale sts vbilling --replicas=0 >/dev/null
wait_for 60 no_pods
DOWN_UNTIL=$(minute 120)
wait_for 200 "after $DOWN_UNTIL"
kubectl -n vbilling scale sts vbilling --replicas=1 >/dev/null
kubectl -n vbilling rollout status sts/vbilling --timeout=120s >/dev/null
for p in "${PIDS[@]:-}"; do kill "$p" 2>/dev/null || true; done; PIDS=()
forward vbilling vbilling 28181 8080
forward monitoring prometheus 29191 9090
END=$(minute 0)
check "every source catches up after the outage" "wait_for 180 caught_up"

step "Exactness"
api "/api/v1/events?from=$START&to=$END&limit=100000" >"$WORK/events.jsonl"
python3 -I - "$WORK/events.jsonl" >"$WORK/totals.json" <<'EOF'
import json, sys
t = {"gateway": 0, "late": 0, "engine": 0, "estimate": 0, "estimates_marked": True, "ids": set(), "dups": 0}
for line in open(sys.argv[1]):
    if not line.startswith("{"): continue
    e = json.loads(line)
    if e["id"] in t["ids"]: t["dups"] += 1
    t["ids"].add(e["id"])
    p = e.get("properties") or {}
    if e["metric"] == "inference_output_tokens":
        t["gateway"] += e["quantity"]; t["late"] += e["quantity"] if p.get("late") else 0
    elif e["metric"] == "engine_output_tokens":
        t["engine"] += e["quantity"]
    elif e["metric"] == "engine_output_tokens_est":
        t["estimate"] += e["quantity"]; t["estimates_marked"] &= p.get("estimate") is True
del t["ids"]
print(json.dumps(t))
EOF
RAW=$(curl -fsS "http://$PROM/api/v1/query" --data-urlencode "query=vllm:generation_tokens_total{namespace=\"vc-team-a\"}[1h]" --data-urlencode "time=$END" |
  python3 -I -c '
import json, sys, datetime
start = datetime.datetime.strptime(sys.argv[1], "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=datetime.timezone.utc).timestamp()
total = 0
for s in json.load(sys.stdin)["data"]["result"]:
    vals = [(float(t), float(v)) for t, v in s["values"]]
    prev = next((v for t, v in reversed(vals) if t <= start), None)
    for t, v in vals:
        if t <= start: continue
        if prev is None: prev = 0.0
        total += (v - prev) if v >= prev else v
        prev = v
print(int(total))' "$START")
cat "$WORK/totals.json"; echo "  Prometheus raw-sample increase: $RAW"
check "the gateway total equals what was sent, retries ignored ($SENT)" '[ "$(total gateway)" = "$SENT" ]'
check "the late record is a separate adjustment to its own window" '[ "$(total late)" = "77" ]'
check "the counter total equals Prometheus' raw increase ($RAW)" '[ "$(total engine)" = "$RAW" ]'
check "query results are marked as estimates" '[ "$(total estimates_marked)" = "true" ]'
check "no event ID repeats" '[ "$(total dups)" = "0" ]'
crosscheck_rows() { api "/api/v1/crosscheck?from=$START&to=$END" | jq -e '.rows | length > 0' >/dev/null; }
counter_ok() { api /api/v1/sources | jq -e '[.sources[] | select(.name | startswith("prometheus/engine_output_tokens/")) | .status] | length > 0 and all(. == "ok")' >/dev/null; }
check "the cross-check compares both sources" crosscheck_rows
check "source health reports the Prometheus counter as ok" counter_ok

step "Enforcement inside the tenant's cluster"
B="http://$API/api/v1/billing-states/acme"
curl -fsS -H "Authorization: Bearer $TOKEN" -X PUT "$B" -d '{"state":"suspended","reason":"e2e unpaid"}' >/dev/null
check "a suspended tenant cannot start pods in its own cluster" "wait_for 30 'refused blocked vbilling-suspended'"
curl -fsS -H "Authorization: Bearer $TOKEN" -X PUT "$B" -d '{"state":"suspended","reason":"e2e review","hold":"abuse"}' >/dev/null
curl -fsS -H "Authorization: Bearer $TOKEN" -X PUT "$B" -d '{"state":"active","reason":"invoice paid"}' >/dev/null
check "a payment does not lift an operator's hold" "wait_for 30 \"refused still-blocked 'abuse: e2e review'\""
curl -fsS -H "Authorization: Bearer $TOKEN" -X PUT "$B" -d '{"state":"active","reason":"reviewed","hold":"abuse"}' >/dev/null
check "lifting the last hold allows pods again" "wait_for 30 'allowed allowed'"

echo; echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
