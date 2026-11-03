#!/bin/sh
set -eu
version=${ROI_VERSION:-$(git describe --tags --always --dirty 2>/dev/null || printf dev)}
commit=${ROI_COMMIT:-$(git rev-parse --verify HEAD 2>/dev/null || printf unknown)}
built=${ROI_BUILD_DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}
case "$version$commit$built" in *[!A-Za-z0-9._:+-]*) printf '%s\n' 'Invalid build metadata' >&2; exit 1;; esac
output=${1:-bin/roi}
mkdir -p "$(dirname "$output")"
CGO_ENABLED=${CGO_ENABLED:-0} go build -trimpath -ldflags="-s -w -X github.com/psvmcc/roi/internal/buildinfo.Version=$version -X github.com/psvmcc/roi/internal/buildinfo.Commit=$commit -X github.com/psvmcc/roi/internal/buildinfo.Date=$built" -o "$output" ./cmd/roi
