#!/usr/bin/env bash
set -eo pipefail

echo "======================================================================"
echo " Global Multi-Currency Digital Wallet - Kubernetes Local Deployment"
echo "======================================================================"

# 1. Check if kubectl is installed
if ! command -v kubectl &>/dev/null; then
  echo "[ERROR] kubectl was not found in PATH. Please install kubectl."
  exit 1
fi

# 2. Check cluster readiness
echo "[*] Checking Kubernetes cluster connectivity..."
if ! kubectl cluster-info &>/dev/null; then
  echo "[WARNING] Kubernetes cluster is not reachable!"
  echo "Please make sure Docker Desktop Kubernetes or minikube is active."
  exit 1
fi
echo "[OK] Kubernetes cluster is connected."
kubectl get nodes

# 3. Prepare images
DOCKER_USER="${DOCKERHUB_USERNAME:-bkojha74}"

echo ""
echo "[*] Tagging microservice images for local Kubernetes..."
docker tag "${DOCKER_USER}/wallet-api-gateway:latest" wallet-system/api-gateway:latest 2>/dev/null || true
docker tag "${DOCKER_USER}/wallet-service:latest" wallet-system/wallet-service:latest 2>/dev/null || true
docker tag "${DOCKER_USER}/wallet-ledger-service:latest" wallet-system/ledger-service:latest 2>/dev/null || true
docker tag "${DOCKER_USER}/wallet-auth-service:latest" wallet-system/auth-service:latest 2>/dev/null || true
docker tag "${DOCKER_USER}/wallet-fx-service:latest" wallet-system/fx-service:latest 2>/dev/null || true
docker tag "${DOCKER_USER}/wallet-logging-service:latest" wallet-system/logging-service:latest 2>/dev/null || true

# 4. Apply manifests in deterministic dependency order
echo ""
echo "[*] Applying Kubernetes Namespace and Secrets..."
kubectl apply -f k8s/01-namespace.yaml
kubectl apply -f k8s/09-configmap-secrets.yaml

echo ""
echo "[*] Applying Infrastructure (MongoDB Replica Set & RabbitMQ Broker)..."
kubectl apply -f k8s/02-mongodb.yaml
kubectl apply -f k8s/06-rabbitmq.yaml

echo ""
echo "[*] Applying Microservices (Ledger, Auth, FX, Wallet, Logging, Gateway)..."
kubectl apply -f k8s/03-ledger-service.yaml
kubectl apply -f k8s/11-auth-service.yaml
kubectl apply -f k8s/12-fx-service.yaml
kubectl apply -f k8s/04-wallet-services.yaml
kubectl apply -f k8s/07-logging-service.yaml
kubectl apply -f k8s/05-api-gateway.yaml

echo ""
echo "[*] Applying Ingress, Autoscaling and Disruption Budgets..."
kubectl apply -f k8s/08-ingress.yaml
kubectl apply -f k8s/10-hpa-pdb.yaml

# 5. Trigger rolling updates
echo ""
echo "[*] Triggering rolling restart on deployments..."
kubectl rollout restart deployment/api-gateway -n banking-system 2>/dev/null || true
kubectl rollout restart deployment/wallet-primary -n banking-system 2>/dev/null || true
kubectl rollout restart deployment/wallet-standby -n banking-system 2>/dev/null || true
kubectl rollout restart deployment/ledger-service -n banking-system 2>/dev/null || true
kubectl rollout restart deployment/auth-service -n banking-system 2>/dev/null || true
kubectl rollout restart deployment/fx-service -n banking-system 2>/dev/null || true
kubectl rollout restart deployment/logging-service -n banking-system 2>/dev/null || true

# 6. Display status
echo ""
echo "======================================================================"
echo " Active Kubernetes Workloads in 'banking-system' Namespace"
echo "======================================================================"
kubectl get pods -n banking-system -o wide
echo ""
kubectl get svc -n banking-system
echo ""
echo "[SUCCESS] Kubernetes deployment applied successfully!"
echo "API Gateway is accessible via NodePort at: http://localhost:30080"
