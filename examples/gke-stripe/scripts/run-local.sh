#!/usr/bin/env bash
# Local demo launch only. The compiled Go binary is the actual vBilling app.
# Keep the foreground terminal open; Ctrl-C stops the collector cleanly.
set +x
set -euo pipefail
umask 077
if [[ $# -ne 0 ]]; then
  echo 'Configure RUN_DIR, HOST_KUBECONFIG, WATCH_NAMESPACE, CLUSTER_NAME, REGION, and optionally STRIPE_KEY_FILE, PRIVATE_TENANTS_FILE; then run this script without arguments.' >&2
  exit 2
fi
: "${RUN_DIR:?Private state directory outside the repository is required}"
: "${HOST_KUBECONFIG:?Path to isolated host kubeconfig required}"
: "${WATCH_NAMESPACE:?Exact shared tenant namespace required}"
: "${CLUSTER_NAME:?Demo GKE cluster name required}"
: "${REGION:?GCP region required}"
SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
BINARY=${VBILLING_BINARY:-$RUN_DIR/vbilling}
[[ -x "$BINARY" ]] || { echo "Build first: go build -o \"\$RUN_DIR/vbilling\" ./cmd/vbilling" >&2; exit 2; }
# Validate private files without printing their values. Generate the API token
# outside Git; it is entered into the local dashboard, not sent to Stripe.
python3 - "$SCRIPT_DIR" "$RUN_DIR" "$HOST_KUBECONFIG" "${STRIPE_KEY_FILE:-}" "${PRIVATE_TENANTS_FILE:-}" <<'PY'
import os, pathlib, re, secrets, sys
sys.path.insert(0, sys.argv[1])
from demo_common import GuardError, outside_git, private_read, require
try:
    root=outside_git(sys.argv[2]); require(root.is_dir(), 'Create RUN_DIR first')
    require(root.stat().st_mode & 0o077 == 0, 'RUN_DIR must have mode 0700')
    private_read(sys.argv[3])
    if sys.argv[4]:
        key=private_read(sys.argv[4]); require(re.fullmatch(r'sk_test_[A-Za-z0-9]+', key) and len(key)>16, 'Only a Stripe sk_test_ secret is accepted')
    if sys.argv[5]: private_read(sys.argv[5])
    token=root/'api-token'
    if not token.exists():
        fd=os.open(token, os.O_WRONLY|os.O_CREAT|os.O_EXCL, 0o600)
        with os.fdopen(fd,'w') as f: f.write(secrets.token_urlsafe(32)+'\n')
    require(bool(private_read(token)), 'API token is empty')
except (GuardError, OSError):
    print('Private path, permissions, or test-key validation failed; no values printed.', file=sys.stderr); sys.exit(2)
PY
LOCK="$RUN_DIR/collector.lock"
mkdir "$LOCK" 2>/dev/null || { echo 'Collector lock exists. Stop the recorded collector; remove the lock only after verifying it is stopped.' >&2; exit 2; }
printf '%s\n' "$$" > "$LOCK/launcher.pid"
child=''
cleanup() { if [[ -n "$child" ]] && kill -0 "$child" 2>/dev/null; then kill -TERM "$child" 2>/dev/null || true; wait "$child" || true; fi; rm -f "$LOCK/launcher.pid" "$LOCK/collector.pid"; rmdir "$LOCK"; }
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
export KUBECONFIG="$HOST_KUBECONFIG" DATA_DIR="$RUN_DIR/ledger-data"
export WATCH_NAMESPACES="$WATCH_NAMESPACE" TENANT_SOURCE=cluster
export TENANT_API=false TENANT_ADMIN_FALLBACK=false TENANT_CLUSTERS_FILE=''
export WATCH_DELETIONS=false CPU_MEMORY_BASIS=requests METER_CONTROL_PLANE=false
export ENFORCEMENT_MODE=observe PROMETHEUS_URL='' PROMETHEUS_METRICS_FILE='' CUSTOM_METRICS_FILE=''
export METER_BY_NAMESPACE=false DEDICATED_NODE_LABEL='' STRIPE_WEBHOOK_SECRET=''
unset ADAPTER TENANT_EXCLUDE_NAMESPACES GPU_RESOURCES GPU_SKU_LABEL CAPACITY_TYPE_LABEL
export COLLECTION_INTERVAL=30s RECONCILE_INTERVAL=15s MAX_BACKFILL=15m
export CLUSTER_NAME REGION DEFAULT_PLAN_CODE="${DEMO_PLAN:-$WATCH_NAMESPACE}" BILLING_CURRENCY=USD
export LISTEN_ADDR=127.0.0.1:18090
API_TOKEN=$(cat "$RUN_DIR/api-token")
export API_TOKEN INGEST_TOKEN=''
export ADAPTERS=noop STRIPE_API_KEY=''
export STRIPE_API_BASE=https://api.stripe.com STRIPE_API_VERSION=2026-09-30.endive
export STRIPE_AUTO_SUBSCRIBE=false STRIPE_CANCEL_ON_REMOVE=false STRIPE_SPLIT_METERS_BY=sku STRIPE_MAX_RPS=5
if [[ -n "${PRIVATE_TENANTS_FILE:-}" ]]; then
  export TENANT_API=true PRIVATE_NODE_BILLING=node TENANT_CLUSTERS_FILE="$PRIVATE_TENANTS_FILE"
fi
if [[ -n "${STRIPE_KEY_FILE:-}" ]]; then
  STRIPE_API_KEY=$(cat "$STRIPE_KEY_FILE")
  export STRIPE_API_KEY ADAPTERS=stripe
fi
printf 'Starting the Go vBilling application at http://127.0.0.1:18090/; destination: %s\n' "$ADAPTERS"
printf 'Dashboard token file: %s/api-token (keep private). Ledger: %s\n' "$RUN_DIR" "$DATA_DIR"
"$BINARY" &
child=$!
printf '%s\n' "$child" > "$LOCK/collector.pid"
wait "$child"
