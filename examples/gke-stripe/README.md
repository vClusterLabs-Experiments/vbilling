# GPU metering on GKE with Stripe

This guide configures a shared L4 GPU on GKE, runs two CUDA workloads in a vCluster, and sends allocated GPU-hours to Stripe test mode. The optional [Private Nodes guide](PRIVATE-NODES.md) adds a dedicated A100 worker.

This example uses real, paid GCP hardware and synthetic CUDA workloads. Stripe is test-only. It demonstrates allocated GPU-hours, not GPU utilization or real payment collection.

The workflow covers cluster provisioning, metering configuration, test pricing, usage verification, and cleanup. See [validation and limits](#validation-and-limits) for the scope of testing.

## What you will build

```text
Your computer                                      GCP
┌──────────────────────────────────┐               ┌───────────────────────────┐
│ One Go vBilling application      │ ← reads API ─ │ GKE host                  │
│ collector → ledger → dispatcher  │               │ CPU: shared-l4 API        │
│                       ↓          │               │ L4: 2 tenant CUDA pods    │
│                 Stripe adapter   │               │     2 slots / 1 real GPU  │
│ API + dashboard: 127.0.0.1:18090  │               └───────────────────────────┘
└───────────────────────┬──────────┘
                        ↓ quantity, customer, time, event ID
                   Stripe test mode
            Customer + Meter + Product + Price + Subscription
```

The collector and Stripe adapter are parts of the same Go process. The shell launcher starts that binary. The Python Stripe helper only sets up test objects and reads previews; it is not the vBilling application.

This simpler example differs from [examples/gke-gpu](../gke-gpu/README.md): two L4 slots instead of four, local vBilling instead of Helm, no Prometheus, no DCGM query and explicit test pricing with auto-subscription disabled. Do not mix their slot counts or expected quantities.

## Before starting

You need installed and authenticated `gcloud` (including the GKE auth plugin), `kubectl`, `helm`, `vcluster`, Go 1.22 or later, Python 3 and a shell. This guide does not install them. You need permission to create GKE, VPC/subnet, a node service account and its IAM binding, and sufficient L4 quota/capacity. Private Nodes also needs A100 quota and a compatible licensed vCluster Platform.

Set a resource budget and cleanup deadline before provisioning. GPU, disk, IP and load-balancer resources cost money while idle. GCP budgets are alerts, not hard spending caps. This guide does not schedule deletion automatically.

Use a dedicated Stripe test account/sandbox and an `sk_test_` secret stored outside this checkout. Do not use a live key. Do not put a key in a command argument, YAML, terminal recording or Git. Never run these scripts with shell tracing (`set -x`).

Run from the repository root:

```bash
export REPO="$PWD"
export DEMO="$REPO/examples/gke-stripe"
export PROJECT='your-demo-project'
export REGION='us-central1'
export ZONE='us-central1-a'
export RUN_ID="vbdemo-$(date -u +%Y%m%d-%H%M%S)"
export RUN_DIR="$HOME/.local/state/vbilling-demo/$RUN_ID"
export STRIPE_KEY_FILE="$HOME/.config/vbilling/stripe-test-key"
export CLIENT_CIDR='your-current-public-ip/32'
export SHARED='shared-l4'
export SHARED_NS="$RUN_ID"
export NETWORK="$RUN_ID-net"
export SUBNET="$RUN_ID-subnet"
export NODE_SA="$RUN_ID-node@$PROJECT.iam.gserviceaccount.com"
umask 077
mkdir -p "$RUN_DIR/evidence"
export KUBECONFIG="$RUN_DIR/host.kubeconfig"
```

Replace the project, Stripe key-file path and public-IP placeholder. Put the key in its file using your secret manager or editor, then `chmod 600 "$STRIPE_KEY_FILE"`. The run directory contains credentials and must remain outside the checkout. Save your nonsecret variable values privately so a new terminal can resume this exact run; do not generate a new `RUN_ID` while resuming.

Read-only preflight:

```bash
gcloud auth list --filter=status:ACTIVE --format='value(account)'
gcloud projects describe "$PROJECT" --format='value(projectId)'
gcloud services list --project "$PROJECT" --enabled \
  --filter='name:(container.googleapis.com OR compute.googleapis.com)' --format='value(name)'
gcloud container get-server-config --project "$PROJECT" --zone "$ZONE" \
  --format='value(validMasterVersions)'
kubectl version --client
vcluster version
helm version --short
go version
python3 --version
```

Both Compute Engine and Kubernetes Engine APIs must be enabled by the project owner. Check quota and current pricing in the GCP console. The reference configuration uses vCluster chart **0.36.1**, tenant Kubernetes **v1.34.1**, GKE **1.34.11-gke.1209000**, and GPU Operator **v26.7.2**. The commands below use GKE's regular channel because an old patch may no longer be offered; check compatibility before provisioning. Do not assume a newer vCluster CLI/chart works with an older Platform.

## 1. Create an isolated GKE host and L4 pool

These commands create paid resources. The CIDRs below must not overlap networks you need to reach. All names belong only to this run. Stop on an error; inspect the failed step before retrying it.

```bash
gcloud iam service-accounts create "$RUN_ID-node" --project "$PROJECT" \
  --display-name="vBilling demo $RUN_ID"
gcloud projects add-iam-policy-binding "$PROJECT" \
  --member="serviceAccount:$NODE_SA" \
  --role=roles/container.defaultNodeServiceAccount --condition=None

gcloud compute networks create "$NETWORK" --project "$PROJECT" \
  --subnet-mode=custom --bgp-routing-mode=regional --description="vbilling-demo:$RUN_ID"
gcloud compute networks subnets create "$SUBNET" --project "$PROJECT" \
  --region "$REGION" --network "$NETWORK" --range=10.240.0.0/24 \
  --secondary-range=pods=10.244.0.0/16,services=10.245.0.0/20 \
  --enable-private-ip-google-access --description="vbilling-demo:$RUN_ID"

gcloud container clusters create "$RUN_ID" --project "$PROJECT" --zone "$ZONE" \
  --node-locations "$ZONE" --release-channel regular \
  --num-nodes=1 --machine-type=e2-standard-2 --image-type=COS_CONTAINERD \
  --disk-type=pd-balanced --disk-size=50 \
  --network "$NETWORK" --subnetwork "$SUBNET" --enable-ip-alias \
  --cluster-secondary-range-name=pods --services-secondary-range-name=services \
  --enable-master-authorized-networks --master-authorized-networks "$CLIENT_CIDR" \
  --service-account "$NODE_SA" --scopes=cloud-platform \
  --workload-pool "$PROJECT.svc.id.goog" --workload-metadata=GKE_METADATA \
  --addons=GcePersistentDiskCsiDriver --logging=SYSTEM --monitoring=SYSTEM \
  --no-enable-managed-prometheus --max-surge-upgrade=0 --max-unavailable-upgrade=1 \
  --labels="purpose=vbilling-demo,run=$RUN_ID"

gcloud container node-pools create l4-shared --project "$PROJECT" \
  --cluster "$RUN_ID" --zone "$ZONE" --node-locations "$ZONE" \
  --num-nodes=1 --machine-type=g2-standard-4 \
  --accelerator=type=nvidia-l4,count=1,gpu-sharing-strategy=time-sharing,max-shared-clients-per-gpu=2,gpu-driver-version=default \
  --image-type=COS_CONTAINERD --disk-type=pd-balanced --disk-size=50 \
  --service-account "$NODE_SA" --scopes=cloud-platform --workload-metadata=GKE_METADATA \
  --node-taints=nvidia.com/gpu=present:NoSchedule \
  --max-surge-upgrade=0 --max-unavailable-upgrade=1 --reservation-affinity=none

gcloud container clusters get-credentials "$RUN_ID" --project "$PROJECT" --zone "$ZONE"
kubectl wait --for=condition=Ready nodes --all --timeout=15m
kubectl get nodes -o wide
kubectl get nodes -l cloud.google.com/gke-nodepool=l4-shared \
  -o 'custom-columns=NAME:.metadata.name,GPU-SLOTS:.status.allocatable.nvidia\.com/gpu'
```

Expected: one Ready CPU node and one Ready GPU node. The GPU node eventually advertises **2** slots. This is one physical L4, not two cards. Driver/device-plugin startup can take additional minutes after the Node first becomes Ready. Do not proceed until both slots are visible.

Capture resource identities for later guarded cleanup:

```bash
python3 "$DEMO/scripts/cleanup-gcp.py" --state "$RUN_DIR/gcp-cleanup.json" capture \
  --project "$PROJECT" --run "$RUN_ID" --zone "$ZONE" --region "$REGION"
```

## 2. Create the shared vCluster

Use the Helm driver and an isolated CLI config. Shared nodes do not need Platform registration for this demo.

```bash
vcluster create "$SHARED" --namespace "$SHARED_NS" --driver=helm \
  --chart-version=0.36.1 --add=false --connect=false \
  --config "$RUN_DIR/vcluster-cli.json" -f "$DEMO/manifests/shared-vcluster.yaml"
kubectl -n "$SHARED_NS" rollout status statefulset/"$SHARED" --timeout=10m
kubectl -n "$SHARED_NS" get pods,pvc -o wide

# A predictable demo identity; required by this helper's ownership guard.
kubectl annotate namespace "$SHARED_NS" \
  "vbilling.vcluster.com/tenant=$RUN_ID" \
  'vbilling.vcluster.com/display-name=Shared L4 example - Stripe test mode'

tk() {
  vcluster connect "$SHARED" -n "$SHARED_NS" --driver=helm \
    --background-proxy=false --config "$RUN_DIR/vcluster-cli.json" -- kubectl "$@"
}
tk get namespaces
```

Expected: the vCluster control-plane pod is Ready on the CPU pool, its PVC is Bound, and the tenant API responds. The annotation is optional in the product, but keep it for this recipe: the test helper intentionally accepts only this run ID or this run ID plus `-private`. Without an annotation, the product derives `vcluster-<namespace>-<vcluster-name>`. The helper accepts explicit run IDs to keep its Stripe operations scoped to this example.

## 3. Run two real CUDA workloads

```bash
tk apply -f "$DEMO/manifests/shared-workload.yaml"
tk -n gpu-demo rollout status deployment/gpu-nbody --timeout=10m
tk -n gpu-demo get pods -o wide
tk -n gpu-demo logs -l app=gpu-nbody --all-containers=true --prefix --tail=40 \
  > "$RUN_DIR/evidence/shared-cuda.txt"
cat "$RUN_DIR/evidence/shared-cuda.txt"
for POD in $(tk -n gpu-demo get pods -l app=gpu-nbody -o name); do
  printf '%s\n' "$POD"
  tk -n gpu-demo exec "$POD" -- nvidia-smi -L
done
kubectl -n "$SHARED_NS" get pods -o wide
```

Expected: two Ready workload pods, real L4 device output, successful CUDA sample output and the same GPU UUID when queried in both pods. The workload fails if CUDA fails; it does not hide errors. The synced pods run on the host's L4 node while the vCluster control plane stays on CPU.

## 4. Build and start the Go application

Build outside the repository so no binary or runtime data becomes a Git change:

```bash
cd "$REPO"
go build -o "$RUN_DIR/vbilling" ./cmd/vbilling
```

The launcher takes environment variables and no arguments. Its required inputs are the binary, host kubeconfig, shared namespace, run directory and Stripe test-key file for this Stripe demo. The launcher keeps the UI on loopback, creates a separate API token in the run directory and uses the measurement settings below. It starts **one Go process**. Leave its terminal running; use another terminal with the same saved variables for the checks and Stripe setup.

```bash
export HOST_KUBECONFIG="$RUN_DIR/host.kubeconfig"
export WATCH_NAMESPACE="$SHARED_NS"
export CLUSTER_NAME="$RUN_ID"
"$DEMO/scripts/run-local.sh"
```

Expected startup: `destination: stripe` and `http://127.0.0.1:18090/`. If it says `noop`, the Stripe key-file variable was not set; set the variable and restart to enable Stripe delivery. The ledger is `$RUN_DIR/ledger-data`. The launcher prevents a second process for the same run directory; it cannot detect a different run directory collecting the same cluster.

The application uses these measurement and delivery settings:

| Setting | Example value | Why |
|---|---|---|
| `ADAPTERS` | `stripe` | Send records to Stripe |
| `STRIPE_API_KEY` | Loaded privately from the test key file | Authenticate; never print it |
| `STRIPE_API_BASE` | `https://api.stripe.com` | Stripe endpoint |
| `STRIPE_API_VERSION` | `2026-09-30.endive` | Repository's API version |
| `STRIPE_SPLIT_METERS_BY` | `sku` | Separate L4 and A100 meters |
| `STRIPE_AUTO_SUBSCRIBE` | `false` | The helper creates test pricing and the subscription |
| `STRIPE_CANCEL_ON_REMOVE` | `false` | Deleting a tenant does not cancel unrelated billing setup |
| `COLLECTION_INTERVAL` | `30s` | Collect a usage window every 30 seconds |
| `CPU_MEMORY_BASIS` | `requests` | Requested CPU/memory, not consumption |
| `METER_CONTROL_PLANE` | `false` | Exclude vCluster control-plane resources |
| `PROMETHEUS_URL` | Empty | No Prometheus input for this demo |
| `WATCH_NAMESPACES` | The shared host namespace only | Limit host discovery |
| `ENFORCEMENT_MODE` | `observe` | No billing-driven suspension |

The test key authorizes Stripe. The API token protects the local dashboard and HTTP API. They are different secrets; do not paste the Stripe key into the dashboard token field.

In the second terminal, define the read helper and query the application:

```bash
api() {
  python3 "$DEMO/scripts/api-read.py" --token-file "$RUN_DIR/api-token" --path "$1"
}
curl --fail http://127.0.0.1:18090/healthz
api /api/v1/status
api /api/v1/tenants
api '/api/v1/events?metric=vcluster_gpu_hours&limit=10'
api '/api/v1/usage?group_by=tenant,sku&metric=vcluster_gpu_hours'
api /api/v1/ledger/verify
api /api/v1/destinations
api '/api/v1/events?metric=vcluster_gpu_hours&limit=10' > "$RUN_DIR/evidence/gpu-events.json"
```

Open **http://127.0.0.1:18090/**. The dashboard asks for the token in `$RUN_DIR/api-token`. Read that private file locally and enter its value in the dashboard. Do not include the token in logs, screenshots, or shared files.

Expected: `/healthz` responds successfully; `/api/v1/status` reports readiness; the tenant appears; the Stripe destination is healthy; ledger verification passes. After two complete windows, GPU events should show `vcluster_gpu_hours`, SKU `nvidia-l4-timeslice-2`, quantity **0.008333333** per full window:

```text
2 pods × 0.5 GPU each × 30 seconds / 3,600 = 0.008333333 GPU-hours
```

An initial or final partial window can be smaller. Kubernetes gives vBilling resource allocation and lifecycle facts. It does not give a GPU utilization percentage in this flow.

## 5. Set the selling price in Stripe test mode

vBilling creates/finds the Customer and Meter. The helper discovers those exact objects and creates a Product and a Price of USD 2.50 per GPU-hour. It then creates a Subscription and pauses collection. The Price points to the Meter; this is how incoming usage connects to money.

```bash
stripe_demo() {
  python3 "$DEMO/scripts/stripe-test.py" \
    --key-file "$STRIPE_KEY_FILE" --state "$RUN_DIR/stripe-shared.json" \
    --run "$RUN_ID" --tenant "$RUN_ID" \
    --event vcluster_gpu_hours__nvidia_l4_timeslice_2 "$@"
}
stripe_demo catalog
stripe_demo create-and-pause
stripe_demo inspect
```

Expected: only test objects, the correct tenant Customer, the L4 Meter, a Product, a monthly metered Price of 250 US cents and a paused Subscription. The helper keeps created IDs in the private state file, checks ownership and refuses a live key. **Creating and pausing are two separate API requests.** An initial zero-value invoice may exist before the pause. If the pause fails, preserve the state file and rerun `stripe_demo create-and-pause`: it uses the saved Subscription ID to retry the pause. Alternatively run `stripe_demo cancel` to cancel that exact Subscription. Do not delete state or create another subscription.

In the Stripe Dashboard, use test mode and search for the IDs reported by the helper. Meter settings should be SUM, customer payload key `stripe_customer_id`, and value payload key `value`. Product means the service's name; Price means the unit rate; Subscription connects this Customer to that Price.

Wait at least a few full windows **after the Subscription starts**, then:

```bash
stripe_demo preview > "$RUN_DIR/evidence/stripe-shared-preview.json"
cat "$RUN_DIR/evidence/stripe-shared-preview.json"
```

Expected: `livemode=false`, a paused Subscription, fractional GPU-hour usage and a calculated amount. Stripe aggregates asynchronously; a first preview can be empty or zero. `quantity_decimal` is important when quantity is below one; an integer field can show zero. Compare identical customer/meter/time ranges. The total depends on the measured time period; it is not a fixed test expectation.

## 6. Verify usage delivery

Check that both workload pods are Ready and their logs confirm successful CUDA execution on the same physical L4. Compare a complete 30-second GPU-hour event in vBilling with the corresponding Stripe customer and meter. Match the time range when comparing totals; Stripe aggregation can lag behind the local ledger.

The Stripe adapter transforms each GPU-hours record into these request fields:

| Stripe field | Source |
|---|---|
| `event_name` | Metric and SKU, such as `vcluster_gpu_hours__nvidia_l4_timeslice_2` |
| `payload[stripe_customer_id]` | Stripe Customer mapped to the vBilling tenant ID |
| `payload[value]` | Measured GPU-hour quantity |
| `timestamp` | Window start in Unix seconds |
| `identifier` | Stable vBilling event ID used for deduplication |

The adapter creates or finds Customers and Meters. The setup helper separately creates the Product, Price, and paused test Subscription. The Price references the Meter, so Stripe can apply USD 2.50 per GPU-hour to that customer's accumulated usage. The selling price and Price ID are not needed in the vBilling process to deliver usage.

Keep diagnostic exports in `$RUN_DIR/evidence`. The ledger and adapter state remain under `$RUN_DIR/ledger-data`; reuse this directory when restarting the same collector. Do not run overlapping collectors for the same cluster scope.

## 7. Clean up

First save the final evidence and let the latest complete window be delivered. Stop the foreground Go process with Ctrl-C and stop any demo port-forward terminal. Preserve its data and state locally if you need to inspect the run later. Do not stop another person's process with a broad `pkill`.

Cancel only this demo's test subscription after saving its preview:

```bash
stripe_demo cancel
```

The helper does not delete shared/reused meters or send an invoice. Test objects retained for audit do not mean GCP resources were cleaned up.

If you used the optional Helm alternative, delete its `vbilling` namespace too and wait for its PVC to disappear. Remove only the namespaces created for this demo, then wait for GKE-managed disks and load-balancer resources to be removed. If Private Nodes was used, also follow its cleanup section.

```bash
kubectl delete namespace "$SHARED_NS" --wait=true --timeout=10m
kubectl get pv
kubectl get services --all-namespaces
python3 "$DEMO/scripts/cleanup-gcp.py" --state "$RUN_DIR/gcp-cleanup.json" plan
python3 "$DEMO/scripts/cleanup-gcp.py" --state "$RUN_DIR/gcp-cleanup.json" \
  execute --confirm-run "$RUN_ID" --host-kubeconfig "$KUBECONFIG"
```

The cleanup helper checks the captured cloud identities and refuses to delete a GKE cluster while PVs or LoadBalancer Services remain. Do not remove finalizers to make the check pass. Inspect the exact remaining resource and its owner; finish its normal deletion first. The helper deletes only captured demo resources, never all resources in a project. Review its final report for residual resources and inspect the GCP console. Deleting pods alone does not stop GPU VM charges.

Remove any stale Platform registration created for this run separately; see the private guide. Keep key files outside Git and revoke temporary access when the demo is finished.

## Recovery

| Symptom | Next check |
|---|---|
| GKE command fails for quota/capacity | Check the error, quota and regional availability. Do not silently create an extra GPU elsewhere; update the run's zone/cleanup record deliberately. |
| Node Ready but no GPU slots | Wait for the NVIDIA driver/device plugin; inspect system pod status and node allocatable resources. |
| CUDA pods Pending | Inspect `tk -n gpu-demo describe pod <pod>` and host events. Check the pool labels, taint toleration and two-slot capacity. |
| Pod Running but CUDA not successful | Inspect logs and readiness. Do not claim GPU execution from pod phase alone. |
| Mac cannot reach GKE | Verify VPN/current public IP against the explicit master authorized CIDR; change only this run's allowlist if authorized. |
| No tenant or no usage | Check `WATCH_NAMESPACES`, vCluster labels, optional tenant annotation and ownership of synced pods. Then check application logs. |
| Dashboard requests a token | Use the generated local API token, never the Stripe key. Keep the token out of logs and shared files. |
| Stripe customer/meter not found yet | Check destination errors and wait for initial collection/delivery. `catalog` must not invent a customer identity. |
| Stripe preview has no usage | Wait for aggregation and check the Subscription start, attached Price, Meter, Customer and matching time window. |
| Local vBilling stopped | Restart the same binary/config with the same data directory; do not start two collectors against the same scope or erase the ledger. |
| Cleanup reports dependencies | Inspect only this run's PVC/LB/disk resources; wait for normal controller cleanup and rerun the exact captured cleanup plan. |

## Running vBilling in Kubernetes instead

The laptop is just a convenient demo location. The repository's Helm chart starts the same Go application in one StatefulSet pod with persistent storage. Do not run this alongside the local collector against the same workload: that can double-count usage.

```bash
kubectl create namespace vbilling
kubectl -n vbilling create secret generic vbilling-stripe \
  --from-file="api-key=$STRIPE_KEY_FILE"
# Create an API secret from the local API token file; do not paste a token in YAML.
# Use the api-token path generated by run-local.sh.
kubectl -n vbilling create secret generic vbilling-api \
  --from-file="api-token=$RUN_DIR/api-token"
helm upgrade --install vbilling "$REPO/deploy/helm/vbilling" -n vbilling \
  --set 'adapters={stripe}' --set stripe.existingSecret=vbilling-stripe \
  --set stripe.autoSubscribe=false --set stripe.cancelOnRemove=false \
  --set 'stripe.splitMetersBy={sku}' --set api.existingSecret=vbilling-api \
  --set billing.collectionInterval=30s --set metering.cpuMemoryBasis=requests \
  --set metering.meterControlPlane=false --set prometheus.url='' \
  --set "watchNamespaces={$SHARED_NS}" --set tenantAPI.enabled=true
```

This alternative is not an instruction to migrate the active demo's state or start another collector. Plan ledger/state migration and private reader access before switching. Chart settings become pod environment variables; credentials come from Secrets; `/var/lib/vbilling` uses the chart's PVC (default 5Gi). A ConfigMap containing a selling price is not needed for Stripe usage delivery. These are deployment settings, not a full production hardening guide.

## Validation and limits

Validation at source commit `fe58703e2dea63284d95d28a7be71d2a551b40ea` covered shared L4 and private A100 GPU allocation, CUDA execution, ledger integrity, Stripe test usage delivery, and paused invoice previews. The reusable helpers pass 28 offline tests; the Go application builds, and the vCluster and vBilling Helm configurations render successfully. The packaged provisioning workflow has not been rerun end to end in a fresh cloud environment. GPU capacity, API permissions, and version compatibility must be checked for each deployment.

The local runner takes the environment variables shown above and no arguments. Use `python3 scripts/stripe-test.py --help` and `python3 scripts/cleanup-gcp.py --help` from this example directory to inspect the other interfaces. See the [configuration reference](../../docs/configuration.html), [Kubernetes collector](../../internal/metrics/collector.go), [tenant identity discovery](../../internal/discovery/discovery.go), and [Stripe adapter](../../internal/destinations/stripe/adapter.go). This example does not demonstrate DCGM utilization, energy-based pricing, MIG, real invoicing/payment collection, or production availability guarantees.

Run the offline helper checks from the repository root:

```bash
python3 -m unittest discover -s examples/gke-stripe/tests
bash -n examples/gke-stripe/scripts/run-local.sh
```

Before sharing a Git diff, inspect the exact changed files and run the repository's secret scan/checks. Never add the run directory, key files, kubeconfigs, generated join scripts, auth caches, logs or screenshots to Git. A `.gitignore` rule is an extra guard, not proof that a file is safe.
