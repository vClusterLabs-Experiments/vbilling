#!/usr/bin/env bash
set -euo pipefail

# vBilling Demo Script
# Sets up a complete demo environment using vind (vCluster in Docker):
# - vind cluster as the "host" Kubernetes cluster (no kind needed!)
# - Lago (billing engine) via docker-compose
# - Tenant clusters nested inside the vind cluster
# - vBilling controller metering everything

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
VIND_CLUSTER="vbilling-host"
LAGO_DIR="$PROJECT_DIR/deploy/lago"

echo "============================================"
echo " vBilling Demo Setup (powered by vind)"
echo "============================================"
echo ""

# --- Prerequisites check ---
for cmd in docker kubectl vcluster; do
    if ! command -v "$cmd" &>/dev/null; then
        echo "ERROR: $cmd is required but not installed."
        exit 1
    fi
done

# --- Step 1: Create vind cluster (control plane cluster via Docker) ---
echo ">>> Step 1: Creating vind control plane cluster '$VIND_CLUSTER'..."
echo "    This replaces kind - a full K8s cluster runs in Docker via vCluster."
echo ""

# Set docker driver
vcluster use driver docker 2>/dev/null || true

# Check if cluster already exists
if vcluster list --driver docker 2>/dev/null | grep -q "$VIND_CLUSTER"; then
    echo "    vind cluster '$VIND_CLUSTER' already exists, reusing."
    vcluster connect "$VIND_CLUSTER" 2>/dev/null || true
else
    # Create vind cluster with extra worker nodes
    cat > /tmp/vbilling-vind.yaml <<'EOF'
experimental:
  docker:
    nodes:
      - name: worker-1
      - name: worker-2
EOF
    vcluster create "$VIND_CLUSTER" \
        --values /tmp/vbilling-vind.yaml \
        --connect=true
    rm -f /tmp/vbilling-vind.yaml
fi

echo ""
echo "    vind control plane cluster is ready!"
kubectl cluster-info 2>/dev/null || true
kubectl get nodes 2>/dev/null || true
echo ""

# --- Step 2: Install metrics-server in the vind cluster ---
echo ">>> Step 2: Installing metrics-server..."
kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml 2>/dev/null || true
# Patch for vind (skip TLS verification for kubelet)
kubectl patch deployment metrics-server -n kube-system \
    --type='json' \
    -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]' 2>/dev/null || true
echo "    Waiting for metrics-server to be ready..."
kubectl rollout status deployment/metrics-server -n kube-system --timeout=120s 2>/dev/null || true
echo ""

# --- Step 3: Start Lago ---
echo ">>> Step 3: Starting Lago (billing engine)..."
mkdir -p "$LAGO_DIR"

# Lago signs its tokens with an RSA key: generate one for this demo, never commit one.
RSA_KEY="$(openssl genrsa -traditional 2048 2>/dev/null || openssl genrsa 2048 2>/dev/null)"
cat > "$LAGO_DIR/docker-compose.yml" <<'COMPOSE'
version: "3.8"

services:
  db:
    image: postgres:14-alpine
    environment:
      POSTGRES_USER: lago
      POSTGRES_PASSWORD: lago
      POSTGRES_DB: lago
    volumes:
      - lago_pg_data:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U lago"]
      interval: 5s
      timeout: 5s
      retries: 5

  redis:
    image: redis:7-alpine
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 5s
      timeout: 5s
      retries: 5

  api:
    image: getlago/api:v1.17.1
    depends_on:
      db:
        condition: service_healthy
      redis:
        condition: service_healthy
    environment:
      RAILS_ENV: production
      DATABASE_URL: postgresql://lago:lago@db:5432/lago
      REDIS_URL: redis://redis:6379
      SECRET_KEY_BASE: your-secret-key-base-for-demo-only-change-in-production
      LAGO_API_URL: http://localhost:3000
      LAGO_FRONT_URL: http://localhost:8081
      RSA_PRIVATE_KEY: |
        __RSA_PRIVATE_KEY__
      LAGO_ENCRYPTION_PRIMARY_KEY: demo-encryption-primary-key
      LAGO_ENCRYPTION_DETERMINISTIC_KEY: demo-encryption-deterministic-key
      LAGO_ENCRYPTION_KEY_DERIVATION_SALT: demo-encryption-derivation-salt
    ports:
      - "3000:3000"
    command: >
      sh -c "bundle exec rails db:migrate && bundle exec rails s -b 0.0.0.0 -p 3000"

  worker:
    image: getlago/api:v1.17.1
    depends_on:
      db:
        condition: service_healthy
      redis:
        condition: service_healthy
    environment:
      RAILS_ENV: production
      DATABASE_URL: postgresql://lago:lago@db:5432/lago
      REDIS_URL: redis://redis:6379
      SECRET_KEY_BASE: your-secret-key-base-for-demo-only-change-in-production
      LAGO_ENCRYPTION_PRIMARY_KEY: demo-encryption-primary-key
      LAGO_ENCRYPTION_DETERMINISTIC_KEY: demo-encryption-deterministic-key
      LAGO_ENCRYPTION_KEY_DERIVATION_SALT: demo-encryption-derivation-salt
    command: bundle exec sidekiq

  clock:
    image: getlago/api:v1.17.1
    depends_on:
      db:
        condition: service_healthy
      redis:
        condition: service_healthy
    environment:
      RAILS_ENV: production
      DATABASE_URL: postgresql://lago:lago@db:5432/lago
      REDIS_URL: redis://redis:6379
      SECRET_KEY_BASE: your-secret-key-base-for-demo-only-change-in-production
      LAGO_ENCRYPTION_PRIMARY_KEY: demo-encryption-primary-key
      LAGO_ENCRYPTION_DETERMINISTIC_KEY: demo-encryption-deterministic-key
      LAGO_ENCRYPTION_KEY_DERIVATION_SALT: demo-encryption-derivation-salt
    command: bundle exec clockwork lib/clock.rb

  front:
    image: getlago/front:v1.17.1
    depends_on:
      - api
    environment:
      API_URL: http://api:3000
      APP_ENV: production
    ports:
      - "8081:80"

volumes:
  lago_pg_data:
COMPOSE
RSA_KEY="$RSA_KEY" awk '/__RSA_PRIVATE_KEY__/ { n = split(ENVIRON["RSA_KEY"], k, "\n"); for (i = 1; i <= n; i++) print "        " k[i]; next } { print }' \
  "$LAGO_DIR/docker-compose.yml" > "$LAGO_DIR/docker-compose.yml.tmp" && mv "$LAGO_DIR/docker-compose.yml.tmp" "$LAGO_DIR/docker-compose.yml"

cd "$LAGO_DIR"
docker compose up -d
echo "    Waiting for Lago API to be ready..."
for i in $(seq 1 60); do
    if curl -s http://localhost:3000/health > /dev/null 2>&1; then
        echo "    Lago API is ready!"
        break
    fi
    sleep 2
done
echo "    Lago UI: http://localhost:8081"
echo "    Lago API: http://localhost:3000"
echo ""

# --- Step 4: Get Lago API key ---
echo ">>> Step 4: Setting up Lago API key..."
echo "    NOTE: On first use, open http://localhost:8081 to create an organization."
echo "    Then go to Developer > API Keys to get your API key."
echo ""
echo "    For this demo, you can set it via:"
echo "    export LAGO_API_KEY=<your-api-key>"
echo ""

# --- Step 5: Switch to kubernetes driver and create nested tenant clusters ---
echo ">>> Step 5: Creating tenant clusters inside the vind control plane cluster..."
echo "    These tenant clusters run inside vind: the billing targets for vBilling."
echo ""

# Switch driver back to kubernetes for creating nested tenant clusters
vcluster use driver kubernetes 2>/dev/null || true

# Team Alpha - a development team
echo "    Creating vCluster 'team-alpha'..."
vcluster create team-alpha \
    --namespace vcluster-team-alpha \
    --connect=false 2>/dev/null || echo "    team-alpha may already exist"

# Team Beta - a data science team
echo "    Creating vCluster 'team-beta'..."
vcluster create team-beta \
    --namespace vcluster-team-beta \
    --connect=false 2>/dev/null || echo "    team-beta may already exist"

# Team GPU - an ML training team
echo "    Creating vCluster 'team-gpu'..."
vcluster create team-gpu \
    --namespace vcluster-team-gpu \
    --connect=false 2>/dev/null || echo "    team-gpu may already exist"

echo ""
echo "    Tenant clusters created inside the vind control plane cluster:"
kubectl get statefulsets -A -l app=vcluster 2>/dev/null || true
echo ""

# --- Step 6: Deploy some workloads to generate metrics ---
echo ">>> Step 6: Deploying sample workloads for billing..."

# Deploy a simple workload directly in each tenant cluster's namespace of the control plane cluster
# so metrics-server picks it up immediately
for ns in vcluster-team-alpha vcluster-team-beta vcluster-team-gpu; do
    kubectl create namespace "$ns" 2>/dev/null || true
done

# Team Alpha: web app (moderate CPU/memory)
kubectl run web-server -n vcluster-team-alpha \
    --image=nginx:alpine \
    --restart=Always \
    --requests='cpu=100m,memory=128Mi' 2>/dev/null || true

# Team Beta: data processing (higher CPU/memory)
kubectl run data-processor -n vcluster-team-beta \
    --image=busybox:latest \
    --restart=Always \
    --requests='cpu=250m,memory=256Mi' \
    -- sh -c "while true; do echo processing; sleep 10; done" 2>/dev/null || true

# Team GPU: ML workload placeholder (requests CPU for now, GPU in real clusters)
kubectl run ml-trainer -n vcluster-team-gpu \
    --image=busybox:latest \
    --restart=Always \
    --requests='cpu=500m,memory=512Mi' \
    -- sh -c "while true; do echo training; sleep 10; done" 2>/dev/null || true

echo "    Workloads deployed. Waiting for pods to start..."
sleep 5
kubectl get pods -A --field-selector='status.phase=Running' 2>/dev/null | grep -E 'vcluster-team-' || true
echo ""

# --- Step 7: Build vBilling ---
echo ">>> Step 7: Building vBilling..."
cd "$PROJECT_DIR"
make build
echo ""

echo "============================================"
echo " Demo Setup Complete!"
echo "============================================"
echo ""
echo " Architecture:"
echo ""
echo "   Docker"
echo "   ├── vind control plane cluster ($VIND_CLUSTER)"
echo "   │   ├── vCluster: team-alpha  (web workloads)"
echo "   │   ├── vCluster: team-beta   (data processing)"
echo "   │   └── vCluster: team-gpu    (ML training)"
echo "   │"
echo "   └── Lago (billing engine)"
echo "       ├── API:  http://localhost:3000"
echo "       └── UI:   http://localhost:8081"
echo ""
echo "Next steps:"
echo ""
echo "1. Open Lago UI: http://localhost:8081"
echo "   - Create an organization (first-time setup)"
echo "   - Go to Developer > API Keys > copy the API key"
echo ""
echo "2. Run vBilling:"
echo "   export LAGO_API_KEY=<your-api-key>"
echo "   export LAGO_API_URL=http://localhost:3000"
echo "   ./bin/vbilling"
echo ""
echo "3. Watch the magic:"
echo "   - vBilling discovers 3 tenant clusters automatically"
echo "   - Creates billing customers in Lago"
echo "   - Meters CPU, memory, storage every 60s"
echo "   - Check Lago UI > Customers for live billing data"
echo ""
echo "4. Generate more load:"
echo "   vcluster connect team-alpha --namespace vcluster-team-alpha"
echo "   kubectl run stress --image=polinux/stress --restart=Never -- stress --cpu 2 --vm 1 --vm-bytes 256M"
echo "   exit  # disconnects from vCluster"
echo ""
echo "To clean up:"
echo "   vcluster use driver docker"
echo "   vcluster delete $VIND_CLUSTER"
echo "   cd deploy/lago && docker compose down -v"
