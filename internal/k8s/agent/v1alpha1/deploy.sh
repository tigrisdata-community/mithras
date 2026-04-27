#!/usr/bin/env bash

set -euo pipefail

./build.sh

yoke takeoff mithras-agent-v1alpha1 oci://ghcr.io/tigrisdata-community/mithras/crd/agent/airway:v1alpha1 -- --flight-url=oci://ghcr.io/tigrisdata-community/mithras/crd/agent/flight:v1alpha1
