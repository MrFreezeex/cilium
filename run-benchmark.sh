#!/usr/bin/env bash

set -euo pipefail

pause() {
  read -r -p "Press Enter to run the next benchmark..."
}

run_step() {
  local title="$1"
  shift

  echo
  echo "============================================================"
  echo "$title"
  echo "Command: $*"
  echo "============================================================"
  "$@"
}

run_step "Loadbalancer: 15 services, 1 endpoint, 10 iterations" \
  go run ./pkg/loadbalancer/benchmark/cmd -services 15 -endpoints 1 -iterations 10
pause

run_step "Loadbalancer: 15 services, 100 endpoints, 10 iterations" \
  go run ./pkg/loadbalancer/benchmark/cmd -services 15 -endpoints 100 -iterations 10
pause

run_step "Loadbalancer: 15 services, 1000 endpoints, 10 iterations" \
  go run ./pkg/loadbalancer/benchmark/cmd -services 15 -endpoints 1000 -iterations 10
pause

run_step "Loadbalancer: 15 services, 5000 endpoints, 10 iterations" \
  go run ./pkg/loadbalancer/benchmark/cmd -services 15 -endpoints 5000 -iterations 10
pause

run_step "Loadbalancer: 15 services, 10000 endpoints, 10 iterations" \
  go run ./pkg/loadbalancer/benchmark/cmd -services 15 -endpoints 10000 -iterations 10
pause

run_step "Clustermesh: 15 services, 1 backend, 10 iterations" \
  go run ./pkg/clustermesh/benchmark/cmd -services 15 -backends 1 -iterations 10
pause

run_step "Clustermesh: 15 services, 100 backends, 10 iterations" \
  go run ./pkg/clustermesh/benchmark/cmd -services 15 -backends 100 -iterations 10
pause

run_step "Clustermesh: 15 services, 1000 backends, 10 iterations" \
  go run ./pkg/clustermesh/benchmark/cmd -services 15 -backends 1000 -iterations 10
pause

run_step "Clustermesh: 15 services, 5000 backends, 10 iterations" \
  go run ./pkg/clustermesh/benchmark/cmd -services 15 -backends 5000 -iterations 10
pause

run_step "Clustermesh: 15 services, 10000 backends, 10 iterations" \
  go run ./pkg/clustermesh/benchmark/cmd -services 15 -backends 10000 -iterations 10

echo
echo "All benchmark commands completed."
