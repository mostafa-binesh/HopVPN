#!/bin/sh
set -eu
cd "$(dirname "$0")"
exec ./hopport -config ./proxies.json "$@"
