#!/usr/bin/env bash
set -euo pipefail

docker build --network host -t registry.evsaiflow.com/evst/sub2api .
