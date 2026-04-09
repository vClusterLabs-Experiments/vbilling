<p align="center">
  <img src="assets/logo.svg" alt="vBilling" width="400">
</p>

<p align="center">
  <strong>Usage-based billing for vCluster tenants</strong><br>
  Auto-discovers vClusters, meters resource consumption, and generates invoices via <a href="https://github.com/getlago/lago">Lago</a>
</p>

<p align="center">
  <a href="https://github.com/vClusterLabs-Experiments/vbilling/actions"><img src="https://img.shields.io/badge/build-passing-brightgreen" alt="Build"></a>
  <a href="https://goreportcard.com/report/github.com/vClusterLabs-Experiments/vbilling"><img src="https://img.shields.io/badge/go%20report-A+-brightgreen" alt="Go Report"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache%202.0-blue" alt="License"></a>
  <a href="https://github.com/getlago/lago"><img src="https://img.shields.io/badge/billing-Lago%20OSS-06b6d4" alt="Lago"></a>
</p>

---

Built for neoclouds, AI factories, and platform teams running managed Kubernetes with vCluster.

## Architecture

```
┌─────────────────────────────────────────────────────────────────--────┐
│                          Host Cluster                                 │
│                                                                       │
│  ┌──────-────────┐  ┌─-─────────────┐  ┌-──────────────-┐             │
│  │  vCluster     │  │  vCluster     │  │  vCluster      │             │
│  │  team-alpha   │  │  team-beta    │  │  team-gpu      │             │
│  │  (shared)     │  │  (shared)     │  │  (private)     │             │
│  │               │  │               │  │   ┌────────-┐  │             │
│  │  Pods in host │  │  Pods in host │  │   │8× H100  │  │             │
│  │  namespace    │  │  namespace    │  │   │Private  │  │             │
│  │               │  │               │  │   │Nodes    │  │             │
│  └───────┬───────┘  └───────┬───────┘  └───┴─-──┬────┘──┘             │
│          │                  │                   │                     │
│          └──────────────────┼───────────────────┘                     │
│                             │                                         │
│                  ┌──────────▼────────-──┐                             │
│                  │     vBilling         │                             │
│                  │     Controller       │                             │
│                  │                      │                             │
│                  │  • Auto-discovers    │                             │
│                  │  • Collects metrics  │                             │
│                  │  • Sends usage events│                             │
│                  └──────────┬───────────┘                             │
└─────────────────────────────┼────────────────────────────────────────-┘
                              │ Usage events (HTTP)
                     ┌────────▼──────-──┐
                     │      Lago        │
                     │  Billing Engine  │
                     │                  │
                     │  Plans & pricing │  ← Provider configures here
                     │  Subscriptions   │
                     │  Invoices        │
                     │  Wallets/prepay  │
                     └──────────────────┘
```

vBilling handles **metrics collection and event delivery**. Lago handles **pricing, plans, and invoicing**. Providers configure their own pricing in Lago — vBilling never decides what to charge.

## Supported Deployment Models

### Shared Nodes (IDP / Platform Teams)

Multiple vClusters share the same host cluster nodes. Billing is based on actual pod-level resource consumption.

```
Host Node (shared)
├── team-alpha pods → billed by actual CPU/memory usage
├── team-beta pods  → billed by actual CPU/memory usage
└── team-gpu pods   → billed by actual CPU/memory usage
```

### Private Nodes (Neoclouds / AI Factories)

Each tenant gets dedicated bare-metal nodes (GPUs, high-memory, etc.). Billing is based on full node allocation — the entire node is theirs.

```
team-gpu's Private Nodes (dedicated)
├── node-1: 8× H100, 96 CPU, 1TB RAM → billed for full node
├── node-2: 8× H100, 96 CPU, 1TB RAM → billed for full node
└── node-3: 8× H100, 96 CPU, 1TB RAM → billed for full node
```

vBilling detects the mode automatically:
- **Private nodes found** → bills full node capacity (CPU, memory, GPUs)
- **No private nodes** → bills actual pod-level usage from metrics-server

## What Gets Metered

| Metric | Source | Shared Mode | Private Mode |
|--------|--------|-------------|-------------|
| CPU core-hours | metrics-server / node capacity | Pod usage | Full node capacity |
| Memory GB-hours | metrics-server / node capacity | Pod usage | Full node capacity |
| Storage GB-hours | PVC sizes | Per PVC | Per PVC |
| GPU hours (by type) | Pod requests + node labels | Per pod allocation | Full node GPUs |
| GPU utilization | DCGM via Prometheus | Per GPU % | Per GPU % |
| Network egress GB | Prometheus | Per namespace | Per namespace |
| LoadBalancer hours | Service count | Per LB service | Per LB service |
| Instance hours | Per vCluster flat | 1 per vCluster | 1 per vCluster |
| Private node hours | Node count | N/A | Per dedicated node |

**GPU type detection** reads from node labels:
- `nvidia.com/gpu.product` (NVIDIA GPU Operator)
- `cloud.google.com/gke-accelerator` (GKE)
- `k8s.amazonaws.com/accelerator` (EKS)

An H100 hour and a T4 hour are tracked as separate events so providers can price them differently in Lago.

## Quick Start

### Prerequisites

- Kubernetes cluster with vClusters running
- [metrics-server](https://github.com/kubernetes-sigs/metrics-server) installed
- Lago instance (see [Deploying Lago](#deploying-lago))
- Optional: Prometheus with [DCGM Exporter](https://github.com/NVIDIA/dcgm-exporter) for GPU utilization

### 1. Deploy Lago

```bash
# Clone the vBilling repo (includes Lago docker-compose)
git clone https://github.com/vClusterLabs-Experiments/vbilling.git
cd vbilling/deploy/lago

# Generate RSA key for Lago (required for JWT signing)
openssl genrsa 2048 > lago_rsa.key
openssl rsa -in lago_rsa.key -out lago_rsa.key -traditional 2>/dev/null

# Create .env with Base64-encoded RSA key (Lago expects LAGO_RSA_PRIVATE_KEY)
echo "LAGO_RSA_PRIVATE_KEY=$(base64 -i lago_rsa.key | tr -d '\n')" > .env

# Start Lago
docker compose --env-file .env up -d

# Wait for API to be ready (~30s for database migrations)
# UI: http://localhost:8080 | API: http://localhost:3000
```

Create an organization in Lago (first-time only):
```bash
curl -s -X POST http://localhost:3000/graphql \
  -H "Content-Type: application/json" \
  -d '{"query":"mutation { registerUser(input: { email: \"admin@example.com\", password: \"yourpassword\", organizationName: \"My Org\" }) { token } }"}'

# Get your API key
docker exec lago-db-1 psql -U lago -d lago -t -c "SELECT value FROM api_keys LIMIT 1;"
```

### 2. Install vBilling

**Option A: Helm (production)**
```bash
# Build and push the image first
docker buildx build --platform linux/amd64,linux/arm64 \
  -t <your-registry>/vbilling:v0.1.0 --push .

# Install via Helm
helm upgrade --install vbilling deploy/helm/vbilling \
  --namespace vbilling-system --create-namespace \
  --set image.repository=<your-registry>/vbilling \
  --set image.tag=v0.1.0 \
  --set lago.apiURL=http://lago-api.lago-system:3000 \
  --set lago.apiKey=YOUR_LAGO_API_KEY
```

**Option B: Run locally (development/testing)**
```bash
make build
LAGO_API_KEY=<key> LAGO_API_URL=http://localhost:3000 ./bin/vbilling
```

### 3. Configure Pricing in Lago

vBilling creates billable metrics and a skeleton plan with **$0 pricing**. You set your own prices:

1. Open Lago UI → **Plans** → **vCluster Standard**
2. Edit each charge with your pricing:
   - CPU Core-Hours: `$0.065` (your cost + margin)
   - Memory GB-Hours: `$0.009`
   - GPU Hours: `$4.50` (for H100) or use Lago's graduated pricing for volume discounts
   - Storage GB-Hours: `$0.0002`
   - Network Egress GB: `$0.09`
   - Private Node Hours: `$25.00` (for dedicated node billing)
3. Save — pricing takes effect immediately for all tenants

You can also create **multiple plans** (e.g., "GPU Premium", "Dev Tier") and assign different plans to different customers via the Lago API.

### 4. Done

vBilling will:
- Auto-discover all vClusters (via StatefulSet labels or Platform API)
- Create a billing customer in Lago for each vCluster
- Start sending usage events every 60 seconds
- Lago generates invoices at the end of each billing period

## How It Works

### Discovery

vBilling finds vClusters using two methods:

1. **Label scanning** (works with OSS vCluster): Watches StatefulSets and Deployments with `app=vcluster` label
2. **Platform API** (works with vCluster Platform): Lists `VirtualClusterInstance` resources via the management API

### Metrics Collection Loop

Every collection interval (default 60s):

```
For each discovered vCluster:
  1. Check for private nodes (labels: vcluster.loft.sh/managed-by=<name>)
     → If found: read full node capacity (CPU, memory, GPUs, storage)
     → If not: read pod-level metrics from metrics-server

  2. Collect storage from PVCs in the namespace

  3. Collect GPU allocation from pod nvidia.com/gpu requests
     → Detect GPU type from the node's nvidia.com/gpu.product label

  4. Count LoadBalancer services

  5. Check spot vs on-demand node status for cost attribution

  6. (If Prometheus configured) Query DCGM for GPU utilization
  7. (If Prometheus configured) Query network egress bytes

  8. Convert all metrics to billing units:
     CPU: cores × interval_hours = core-hours
     Memory: GB × interval_hours = GB-hours
     GPU: count × interval_hours = GPU-hours (tagged with GPU type)

  9. Send all events to Lago in batch
```

### Billing Flow

```
vCluster created  →  Customer auto-created in Lago
                  →  Subscription started (plan: vcluster-standard)
                  →  Usage events every 60s
                  →  Lago aggregates over billing period
                  →  Invoice generated (monthly)
                  →  Webhook to payment provider (optional)

vCluster deleted  →  Subscription terminated
                  →  Final prorated invoice
```

## Configuration

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `LAGO_API_URL` | `http://localhost:3000` | Lago API endpoint |
| `LAGO_API_KEY` | (required) | Lago API key |
| `COLLECTION_INTERVAL` | `60s` | How often to scrape metrics |
| `RECONCILE_INTERVAL` | `30s` | How often to discover vClusters |
| `DEFAULT_PLAN_CODE` | `vcluster-standard` | Default Lago plan code |
| `BILLING_CURRENCY` | `USD` | Currency for billing |
| `PROMETHEUS_URL` | (empty) | Prometheus URL for DCGM/network |
| `SPOT_DISCOUNT_PERCENT` | `60` | Discount for pods on spot nodes |

**Note:** Pricing is NOT configured via environment variables. Configure pricing in Lago UI or API.

### Helm Values

```yaml
lago:
  apiURL: "http://lago-api:3000"
  apiKey: ""
  existingSecret: "lago-credentials"  # or use existing K8s secret

billing:
  collectionInterval: "60s"
  reconcileInterval: "30s"

prometheus:
  url: "http://prometheus.monitoring:9090"  # optional
```

## Use Cases

### Neocloud / AI Factory

Each customer gets a vCluster with Private Nodes (dedicated GPU bare metal). vBilling meters the full node allocation.

```
Customer signs up
  → Platform provisions vCluster + Private Nodes (H100 cluster)
  → vBilling discovers vCluster, detects private nodes
  → Sends events: 8 GPU-hours (H100) + 96 CPU-hours + 1TB memory-hours per hour
  → Lago invoices monthly at provider's rates
  → Customer pays via Stripe (Lago webhook integration)
```

### Internal Platform (IDP)

*Example: Enterprise platform team*

Dev teams share cluster resources via vClusters. vBilling enables internal chargeback.

```
Team requests vCluster
  → Platform team creates vCluster (shared nodes)
  → vBilling discovers it, starts metering actual usage
  → Each team sees their cost in Lago customer portal
  → Finance does quarterly chargeback based on Lago invoices
```

## Dashboard

A lightweight billing dashboard is included at `dashboard/index.html`. It queries the Lago API directly and shows per-tenant usage breakdowns.

```bash
# Serve the dashboard
cd dashboard
python3 -m http.server 9090
# Open http://localhost:9090
```

Features:
- Per-tenant usage cards with metric breakdown
- Total spend across all tenants
- Auto-refresh every 30 seconds
- No framework dependencies

## Project Structure

```
cmd/vbilling/main.go              Entry point
internal/
  config/config.go                Configuration from env vars
  lago/
    client.go                     Lago HTTP API client
    bootstrap.go                  Auto-creates metrics + skeleton plan
  discovery/discovery.go          vCluster discovery (labels + Platform API)
  metrics/collector.go            All metrics: CPU, memory, GPU, storage,
                                  network, DCGM, private nodes, spot/on-demand
  controller/controller.go        Main reconciliation + billing loop
deploy/
  helm/vbilling/                  Helm chart with RBAC
  lago/                           Docker Compose for Lago
dashboard/index.html              Billing dashboard
scripts/demo.sh                   End-to-end demo using vind
Dockerfile                        Multi-stage distroless build
Makefile                          Build targets
```

## Building

```bash
make build          # Build binary (local OS/arch)
make docker-build   # Build Docker image (local arch)
make test           # Run tests
make helm-install   # Install via Helm
make tidy           # go mod tidy
```

### Multi-Arch Docker Image

For production K8s clusters (linux/amd64) and Apple Silicon (linux/arm64):

```bash
# Build and push multi-arch image
docker buildx create --use --name vbilling-builder 2>/dev/null || true
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  -t <your-registry>/vbilling:v0.1.0 \
  --push .
```

The Dockerfile uses multi-stage build with `gcr.io/distroless/static:nonroot` as the final image (~10MB).

## Deploying Lago

### Docker Compose (Development/Demo)

```bash
cd deploy/lago
docker compose --env-file .env up -d
```

### Kubernetes (Production)

Deploy Lago as Kubernetes workloads. Key components: PostgreSQL, Redis, API (Rails), Sidekiq worker, Clock, Frontend. See [Lago docs](https://getlago.com/docs) for production guidance.

## Roadmap

- [ ] MIG (Multi-Instance GPU) partition tracking
- [ ] Lago webhook handler for invoice lifecycle events
- [ ] Grafana dashboard integration
- [ ] Budget alerts per vCluster
- [ ] Reserved capacity / commitment pricing
- [ ] Auto Nodes billing (dynamic node provisioning events)
- [ ] Netris network isolation billing integration

## License

Apache 2.0
