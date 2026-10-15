#!/bin/sh
set -eu
cd "$(dirname "$0")/../.."

./scripts/package-snapshot.sh
