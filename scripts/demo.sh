#!/usr/bin/env bash
set -euo pipefail

# vBilling Demo Script
# Sets up a complete demo environment using vind (vCluster in Docker):
# - vind cluster as the "host" Kubernetes cluster (no kind needed!)
# - Lago (billing engine) via docker-compose
# - Nested vClusters inside the vind cluster
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

# --- Step 1: Create vind cluster (host cluster via Docker) ---
echo ">>> Step 1: Creating vind host cluster '$VIND_CLUSTER'..."
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
echo "    vind host cluster is ready!"
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
      LAGO_FRONT_URL: http://localhost:8080
      RSA_PRIVATE_KEY: |
        -----BEGIN RSA PRIVATE KEY-----
        MIIEowIBAAKCAQEA0Z3VS5JJcds3xfn/yGJB+W7mmMCD+sCGpjdOdLqhwFOdhsrB
        UtIhcezLbfCHA2Dc3bMwj7NPHM2GIFvZMh6tQ18BG9VLbgBrplIaS3VNwfdYLlWF
        VNGURYDF0mWXw9MxXR7a+kT8GbdQMP7j62riBsTELY1SFSYB/FEM6gZlPysl7vEq
        ktSpPbCIKbhsfGRaBJAjS0ISlvCi/l4o/JKMoXDbnGaPwbps/ZuMWjPqC0Nv2Y2B
        MTbUUfR7kzVGUVQ7d/FY0vYxqDEsBQ6P7MhmN0JG/uaF3VbUfVpR9P+0Bmby+E2T
        E1AQMRPN5jBseXlqwA3CkFsDJnAP7BfLBcCe8QIDAQABAoIBAE2W7Fz6cV3kQPbC
        r3PvEa4B+c8VZF8RR1G0jWGCN+bCMi+o3yB7SXQKKQGZ3D3P1GWI4CTZY3EThG0
        oOzfFYXC/Rd09+bHxTBylHFdKRwTnaY5dStk+YjZEy1MNKFL4UcGIB94msGJzPEG
        F0aBhxTqBSCkzZ3ID+NP3sRk5ZIlx+RqGqr0FFCIYgVBT3A8AS/XRaOL1END/yOo
        SU7K9+zzW6sL6dqR3VIoK0vVkfxYX3mFzVISYYRvJO6kHU8HHaLFLCHzGlnxfJl7
        EQIuP42K3Bl3m7vX3MFN7i9N7a/FNNqXHBBYJBizGQ8dKf+a3u0P1zAj3o+y1K0E
        mMEZ2QECgYEA56smzRdNllokVsKuGbBx5IRxlOjxjT9MEVXF3JCCYqe2+3Ku0xJK
        OUBlr7GD6pFjw1FBBTCDFX+t0j+xrMmia5dFtAPjZ78VLe/1mlp90B24ezPOclfa
        bYGwlFDI2FTQJBYa/i7T4M7VbL7s+n4V2XfyPJJYzZ+PuDDhnJ4EoECgYEA53T1
        T5mf7APvGa2j5b0W/g5KPHsrM6V9j0pNsMY7RYTLvlMkxDEgYrfMinYx6YWP9DEh
        h2VU7Pq6VkCF+y6MiSIF0l4tXwK7C7xOSUCB1MfmXz/BOJPyGiw7PBqsW/R9A6u
        G7N+G7kj7nKl1OZVbn3YZkjlm/U/e2nR1F+BhmECgYBkZ+hs5rlWp2dnVv/f0GhI
        ihbR+MVP0zR5BPNpNPiIxx6BTZJNPU3x4W5eUaNfPqfV0ovGpWSP/hue1pS5LfTf
        Y5N1MwHXlpYTFNYHXqZS0oB3mTBhxDLxI07CVGd+RXWmpHBfEgz6KOKVGL7IYR+d
        n2xRMPJ5xpN3xT+D08F3AQKBgCoNNgNJ4MGBA1NJia9lM+EBvI9FKb+09MjQFfl1
        t3bCqRhLmsLfId9+BGlhY3L6pq3l+r3di2YhxtRBr/B3mKT4xZgoB78DFc7zuH9I
        CILMi7C5I2JwIgaFF9uqjPdi6gW+vmJbPk5MTAd/yHp2SohcEJFVtKAHQ8GRWWN/
        2z2hAoGBAMsHqW/dwLe0p/LrhGqEPp3iiKcCSvZLfJSe3h9cQ1SmFiPVbMgPcEaJ
        kfDEsi9VwT31mLNjVdNjBBh5LSb0S2lInJC3dcSn3BN+WYnJH1SOEH7OYLZQ+Jxz
        bGiW1JqQ5dIJdMrm/5+BRY87qB7TRPFhHqUOqaxMiZwH/ZVvuKR6
        -----END RSA PRIVATE KEY-----
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
      - "8080:80"

volumes:
  lago_pg_data:
COMPOSE

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
echo "    Lago UI: http://localhost:8080"
echo "    Lago API: http://localhost:3000"
echo ""

# --- Step 4: Get Lago API key ---
echo ">>> Step 4: Setting up Lago API key..."
echo "    NOTE: On first use, open http://localhost:8080 to create an organization."
echo "    Then go to Developer > API Keys to get your API key."
echo ""
echo "    For this demo, you can set it via:"
echo "    export LAGO_API_KEY=<your-api-key>"
echo ""

# --- Step 5: Switch to kubernetes driver and create nested vClusters ---
echo ">>> Step 5: Creating nested vClusters inside the vind host cluster..."
echo "    These are vClusters-inside-vCluster - billing targets for vBilling."
echo ""

# Switch driver back to kubernetes for creating nested vClusters
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
echo "    Nested vClusters created inside vind host:"
kubectl get statefulsets -A -l app=vcluster 2>/dev/null || true
echo ""

# --- Step 6: Deploy some workloads to generate metrics ---
echo ">>> Step 6: Deploying sample workloads for billing..."

# Deploy a simple workload directly in each vCluster's host namespace
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
echo "   ├── vind host cluster ($VIND_CLUSTER)"
echo "   │   ├── vCluster: team-alpha  (web workloads)"
echo "   │   ├── vCluster: team-beta   (data processing)"
echo "   │   └── vCluster: team-gpu    (ML training)"
echo "   │"
echo "   └── Lago (billing engine)"
echo "       ├── API:  http://localhost:3000"
echo "       └── UI:   http://localhost:8080"
echo ""
echo "Next steps:"
echo ""
echo "1. Open Lago UI: http://localhost:8080"
echo "   - Create an organization (first-time setup)"
echo "   - Go to Developer > API Keys > copy the API key"
echo ""
echo "2. Run vBilling:"
echo "   export LAGO_API_KEY=<your-api-key>"
echo "   export LAGO_API_URL=http://localhost:3000"
echo "   ./bin/vbilling"
echo ""
echo "3. Watch the magic:"
echo "   - vBilling discovers 3 vClusters automatically"
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
