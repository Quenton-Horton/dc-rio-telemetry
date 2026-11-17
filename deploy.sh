#!/usr/bin/env bash
set -e

echo "1. Cleaning up duplicate _2 and legacy files..."
rm -f dc_rio_2.go fetchers_2.go index_2.html dc-rio-dashboard_2.json grafana-deployment_2.yaml grafana-provisioning_2.yaml prom-deployment_2.yaml prometheus-config_2.yaml RioWidget.go telemetry-cron.yaml

echo "2. Syncing Go module dependencies..."
go mod tidy

echo "3. Building dc-rio-telemetry:v15..."
docker build --no-cache -t dc-rio-telemetry:v15 -f dockerfile .

echo "4. Importing image into k3d..."
k3d image import dc-rio-telemetry:v15 -c telemetry-cluster

echo "5. Applying Kubernetes manifests..."
kubectl apply -f grafana-provisioning.yaml
kubectl apply -f grafana-deployment.yaml
kubectl apply -f prometheus-config.yaml
kubectl apply -f prom-deployment.yaml
kubectl apply -f telemetry-deployment.yaml
kubectl apply -f telemetry-service.yaml

echo "6. Waiting for rollout..."
kubectl rollout status deployment/logistics-telemetry-worker -n lifestyle-telemetry

echo "Done! Run: kubectl port-forward svc/logistics-telemetry-svc 8080:8080 -n lifestyle-telemetry"
