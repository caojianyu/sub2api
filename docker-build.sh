#!/usr/bin/env bash
set -euo pipefail

docker build --network host -t evst/sub2api:latest .
