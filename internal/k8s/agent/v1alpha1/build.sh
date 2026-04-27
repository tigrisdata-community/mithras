#!/usr/bin/env bash

set -euo pipefail
set -x

export GOOS=wasip1
export GOARCH=wasm
export AWS_PROFILE=tigris

go build -o agent-airway.wasm ./airway
go build -o agent-flight.wasm ./flight
