#!/usr/bin/env bash

set -o errexit -o nounset -o pipefail

# Build the integration test image
echo "Building integration test image..."
docker build -t test-integration --target test-integration .

# Run the integration tests
echo "Running integration tests..."
docker run -t --rm --privileged test-integration ./hack/test-integration.sh
