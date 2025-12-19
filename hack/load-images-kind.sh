#!/bin/bash
# script to pull and load images into kind cluster in local development environment
set -e

IMAGES=(
  "quay.io/jetstack/cert-manager-cainjector:v1.19.1"
  "quay.io/jetstack/cert-manager-controller:v1.19.1"
  "quay.io/jetstack/cert-manager-webhook:v1.19.1"
  "curlimages/curl:latest"
  "valkey/valkey:9.0.0"
)

for IMAGE in "${IMAGES[@]}"; do
  docker pull "$IMAGE"
done

kind load docker-image "${IMAGES[@]}" --name valkey-operator-test-e2e
