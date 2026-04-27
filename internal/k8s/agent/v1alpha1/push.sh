#!/usr/bin/env bash

set -euo pipefail
set -x

./build.sh

yoke stow ./agent-airway.wasm oci://ghcr.io/tigrisdata-community/mithras/crd/agent/airway:v1alpha1
yoke stow ./agent-flight.wasm oci://ghcr.io/tigrisdata-community/mithras/crd/agent/flight:v1alpha1
