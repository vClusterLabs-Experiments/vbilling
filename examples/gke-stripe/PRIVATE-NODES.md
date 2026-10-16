# Optional: a real A100 as a private worker

Complete the shared-node [README](README.md) first. This adds a separate paid A100 VM. The shared L4 setup works independently of this extension. Obtain a budget and cleanup deadline before running the creation commands.

The private vCluster's control plane runs on the host GKE CPU node. The A100 joins the private vCluster directly; it is not a GKE worker. Its pods are visible through its own Kubernetes API. vBilling uses a read-only tenant kubeconfig to read them and the node. `PRIVATE_NODE_BILLING=node` measures the whole dedicated node, including idle allocation.

The reference configuration uses an A100-SXM4-40GB, whole-node GPU accounting, and a paused Stripe test subscription. It does not configure MIG or Prometheus. See the [validation scope](README.md#validation-and-limits).

## 1. Prepare Platform access and create the tenant

Use the same shell variables and isolated `KUBECONFIG` as the main README. Choose an A100-capable zone in the **same region** as the existing subnet. A100 capacity varies by zone; check availability before provisioning.

```bash
export PRIVATE='private-a100'
export PRIVATE_NS="$RUN_ID-private"
export PRIVATE_ZONE='us-central1-f'
export PLATFORM_URL='https://your-vcluster-platform.example'
export PLATFORM_PROJECT='your-demo-platform-project'

vcluster platform login "$PLATFORM_URL" --config "$RUN_DIR/platform-cli.json"
vcluster create "$PRIVATE" -n "$PRIVATE_NS" --driver=helm \
  --chart-version=0.36.1 --connect=false --add=true --project "$PLATFORM_PROJECT" \
  --config "$RUN_DIR/platform-cli.json" -f "$DEMO/manifests/private-vcluster.yaml"
kubectl -n "$PRIVATE_NS" rollout status statefulset/"$PRIVATE" --timeout=10m
kubectl -n "$PRIVATE_NS" get services,pods,pvc
```

Login uses the browser; never put a Platform key in this command or in Git. Private Nodes needs the correct Platform/license entitlement and permission to register a virtual cluster. A scoped credential may authenticate but still lack registration permission. Chart/CLI 0.36.1 is the reference version; upgrading requires checking Platform compatibility.

Check the selected Platform project. If the create command did not register the tenant in the intended project, add this exact tenant explicitly:

```bash
vcluster platform add vcluster "$PRIVATE" -n "$PRIVATE_NS" \
  --project "$PLATFORM_PROJECT" --import-name "$RUN_ID-private" \
  --config "$RUN_DIR/platform-cli.json"
```

Do not blindly repeat registration when the tenant is already present. Registration may restart the control plane. Wait for it to become Ready before creating a join token. The shared tenant can also be registered for Platform management, but that is optional and independent of shared-node measurement.

The private API Service should receive an internal load-balancer address in `10.240.0.0/24`. Wait until it has an address. The manifest allows that subnet and uses separate pod/service ranges `172.20.0.0/16` and `172.21.0.0/16`; change the ranges before installation if they overlap your environment.

```bash
pk() {
  vcluster connect "$PRIVATE" -n "$PRIVATE_NS" --driver=helm \
    --background-proxy=false --config "$RUN_DIR/platform-cli.json" -- kubectl "$@"
}
pk get nodes
```

An empty node list at this point is expected: no worker has joined yet.

## 2. Create a short-lived join token and the A100 VM

The token is a credential. Write it only to the private run directory. Do not expose it in logs, screenshots, or shared files. The generated startup script contains that credential until it is removed after the node joins. The preparation helper checks that the join URL points to either this private Service endpoint or your configured Platform host. If it rejects another endpoint, inspect the Platform/private-service configuration; do not weaken the guard.

```bash
vcluster connect "$PRIVATE" -n "$PRIVATE_NS" --driver=helm \
  --background-proxy=false --config "$RUN_DIR/platform-cli.json" -- \
  vcluster token create --config "$RUN_DIR/platform-cli.json" --expires=2h > "$RUN_DIR/private-join.txt"

export PRIVATE_API_HOST="$(kubectl -n "$PRIVATE_NS" get service "$PRIVATE" \
  -o jsonpath='{.status.loadBalancer.ingress[0].ip}')"
export PLATFORM_HOST="$(python3 -c 'import os, urllib.parse; print(urllib.parse.urlsplit(os.environ["PLATFORM_URL"]).hostname or "")')"
python3 "$DEMO/scripts/private-startup.py" \
  --join-file "$RUN_DIR/private-join.txt" \
  --allowed-host "$PRIVATE_API_HOST" --allowed-host "$PLATFORM_HOST" \
  --output "$RUN_DIR/private-startup.sh"

gcloud compute instances create "$RUN_ID-a100" --project "$PROJECT" \
  --zone "$PRIVATE_ZONE" --machine-type=a2-highgpu-1g \
  --maintenance-policy=TERMINATE --restart-on-failure \
  --image-family=ubuntu-2204-lts --image-project=ubuntu-os-cloud \
  --boot-disk-size=100GB --boot-disk-type=pd-balanced \
  --network "$NETWORK" --subnet "$SUBNET" \
  --no-service-account --no-scopes \
  --labels="purpose=vbilling-demo,run=$RUN_ID" \
  --metadata-from-file="startup-script=$RUN_DIR/private-startup.sh"
```

Expected: one A100 VM. It boots and joins the private tenant. Wait for the node to appear and become Ready:

```bash
pk get nodes -o wide
pk wait --for=condition=Ready node/"$RUN_ID-a100" --timeout=15m
pk label node "$RUN_ID-a100" \
  node.kubernetes.io/instance-type=a2-highgpu-1g \
  vbilling.vcluster.com/capacity-type=on-demand

gcloud compute instances remove-metadata "$RUN_ID-a100" --project "$PROJECT" \
  --zone "$PRIVATE_ZONE" --keys=startup-script
rm "$RUN_DIR/private-join.txt" "$RUN_DIR/private-startup.sh"
```

If the actual node name differs, obtain it from `pk get nodes` and use that exact name. If it never appears, inspect this VM's startup/serial logs privately, the join token age, Platform/license status, internal endpoint and network access. Never copy a token-bearing startup log into a public issue. Do not repeatedly create new GPU VMs to diagnose the same problem.

Record the private VM alongside the other resources. The cleanup helper must be able to verify this VM's immutable ID before deleting it:

```bash
python3 "$DEMO/scripts/cleanup-gcp.py" --state "$RUN_DIR/gcp-cleanup-private.json" capture \
  --project "$PROJECT" --run "$RUN_ID" --zone "$ZONE" --region "$REGION" \
  --private-zone "$PRIVATE_ZONE"
```

Use this private-inclusive state file for final cleanup, rather than the earlier shared-only file.

## 3. Install the driver and run CUDA

GKE manages its L4 driver in the main demo. This independent A100 worker needs a driver and device plugin. GPU Operator installs them inside the private tenant.

```bash
helm repo add nvidia https://helm.ngc.nvidia.com/nvidia
helm repo update nvidia
vcluster connect "$PRIVATE" -n "$PRIVATE_NS" --driver=helm \
  --background-proxy=false --config "$RUN_DIR/platform-cli.json" -- \
  helm upgrade --install gpu-operator nvidia/gpu-operator --version=v26.7.2 \
    -n gpu-operator --create-namespace -f "$DEMO/manifests/gpu-operator-values.yaml"
pk -n gpu-operator get pods
pk get nodes -o 'custom-columns=NAME:.metadata.name,GPU:.status.allocatable.nvidia\.com/gpu,PRODUCT:.metadata.labels.nvidia\.com/gpu\.product'
```

Wait for the driver/operator pods to become healthy and for node GPU capacity to report **1**. Compiling the driver can take several minutes. Do not configure MIG for this demo.

```bash
pk apply -f "$DEMO/manifests/private-workload.yaml"
pk -n gpu-demo rollout status deployment/private-a100-live --timeout=10m
pk -n gpu-demo logs deployment/private-a100-live \
  > "$RUN_DIR/evidence/private-cuda.txt"
cat "$RUN_DIR/evidence/private-cuda.txt"
```

Expected: a Ready workload and real A100 identity plus successful CUDA output. The sample runs a calculation and keeps its existing GPU allocation; it is not an ML model or a utilization benchmark. Check the manifest's readiness behavior and logs rather than assuming pod phase proves success.

## 4. Give local vBilling a read-only connection

The private manifest exports `vbilling-kubeconfig` in the host namespace. It is a service-account credential restricted to reading nodes, pods, PVCs, Services and relevant resource/metrics objects. Do not export an admin kubeconfig as the collector credential.

```bash
python3 "$DEMO/scripts/private-reader.py" \
  --host-kubeconfig "$KUBECONFIG" --namespace "$PRIVATE_NS" --name "$PRIVATE" \
  --tenant "$RUN_ID-private" --region "$REGION" --run-dir "$RUN_DIR"
```

The helper saves private-reader.kubeconfig and private-tenants.json with restrictive permissions, without printing credentials. It preserves TLS verification and uses the private tenant's DNS identity. Because the Mac/laptop cannot directly reach the internal endpoint, start this tunnel in a separate terminal with the same variables:

```bash
kubectl --kubeconfig "$RUN_DIR/host.kubeconfig" -n "$PRIVATE_NS" \
  port-forward --address=127.0.0.1 svc/"$PRIVATE" 18446:443
```

Leave that terminal running. Verify the reader, without displaying the kubeconfig:

```bash
kubectl --kubeconfig "$RUN_DIR/private-reader.kubeconfig" get nodes -o wide
kubectl --kubeconfig "$RUN_DIR/private-reader.kubeconfig" auth can-i list nodes
kubectl --kubeconfig "$RUN_DIR/private-reader.kubeconfig" auth can-i delete nodes
```

Expected: node Ready; `list nodes` says yes; `delete nodes` says no. Restarting a lost tunnel uses the same port and service. Do not set insecure TLS to make a failed connection work.

Stop the existing foreground vBilling process with Ctrl-C. Keep its data directory. Set the optional tenant file and restart the same Go application:

```bash
export PRIVATE_TENANTS_FILE="$RUN_DIR/private-tenants.json"
"$DEMO/scripts/run-local.sh"
```

The launcher enables tenant API access and whole-node billing for this entry. Host discovery remains restricted to the shared namespace, so the private tenant is added once through its explicit file. This avoids collecting it through two discovery paths.

Expected: a second tenant with ID `$RUN_ID-private`. One full 30-second private window includes:

| Metric | Expected quantity | Basis |
|---|---|---|
| `vcluster_gpu_hours` | `0.008333333` | One full A100 × 30/3600 |
| `vcluster_private_node_hours` | `0.008333333` | One private node × 30/3600 |
| `vcluster_cpu_core_hours` | `0.1` | 12 node cores × 30/3600 |
| `vcluster_memory_gb_hours` | Depends on reported node capacity | Kubernetes capacity, not rounded marketing memory |

The GPU SKU should be `NVIDIA-A100-SXM4-40GB`. The L4 and A100 GPU quantities happen to be equal in this example; tenant IDs and SKUs distinguish them. Only the GPU-hour Meter receives a selling Price in the next step. Other recorded capacity does not automatically become a separate charge.

## 5. Add a separate A100 test price

```bash
stripe_private() {
  python3 "$DEMO/scripts/stripe-test.py" \
    --key-file "$STRIPE_KEY_FILE" --state "$RUN_DIR/stripe-private.json" \
    --run "$RUN_ID" --tenant "$RUN_ID-private" \
    --event vcluster_gpu_hours__nvidia_a100_sxm4_40gb "$@"
}
stripe_private catalog
stripe_private create-and-pause
stripe_private inspect
# After full windows and Stripe aggregation:
stripe_private preview > "$RUN_DIR/evidence/stripe-private-preview.json"
```

The helper uses a separate Product/Price/Subscription for the private customer's A100 Meter. Its USD 2.50 rate is an example, not A100 cloud cost. The two-request create/pause caveat and test-only restrictions in the main README apply here too.

## 6. Clean up the private addition and the host

After saving evidence, stop vBilling and the tunnel. Cancel both demo subscriptions using their respective saved states. Do not cancel another tenant's subscription.

```bash
stripe_private cancel
stripe_demo cancel
kubectl delete namespace "$PRIVATE_NS" "$SHARED_NS" --wait=true --timeout=10m
kubectl get pv
kubectl get services --all-namespaces
python3 "$DEMO/scripts/cleanup-gcp.py" --state "$RUN_DIR/gcp-cleanup-private.json" plan
python3 "$DEMO/scripts/cleanup-gcp.py" --state "$RUN_DIR/gcp-cleanup-private.json" \
  execute --confirm-run "$RUN_ID" --host-kubeconfig "$KUBECONFIG"
```

Use the state file that includes the A100. The helper will delete the captured A100 VM and the shared host resources after ownership/dependency checks. Review its residual-resource report, including disks and load balancers. If a VM deadline already deleted the A100, verify that fact rather than creating a replacement.

Finally remove this run's imported virtual-cluster entry from the Platform project through the Platform UI. Select the exact registration created for this run, verify the run ID and confirm the infrastructure has already been removed. Do not delete a shared host registration or another virtual cluster. Retain only the evidence you need in the private run directory; do not commit kubeconfigs, Platform auth files or generated join scripts.
