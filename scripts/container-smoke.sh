#!/bin/sh
# Run the exact image that will be published, with temporary state only.
set -eu
engine=${CONTAINER_ENGINE:-docker}
image=${1:?Usage: container-smoke.sh IMAGE}
name="roi-smoke-$$"
work=$(mktemp -d)
# Called by the EXIT trap.
# shellcheck disable=SC2329
cleanup() {
    "$engine" rm -f "$name" >/dev/null 2>&1 || true
    rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 1' HUP INT TERM
sed -e 's/127.0.0.1:/0.0.0.0:/g' -e 's#./data/roi.db#/data/roi.db#' examples/config.yaml > "$work/config.yaml"
"$engine" run --rm -v "$work/config.yaml:/config/config.yaml:ro" -e ROI_CHECK_CONFIG=true "$image"
"$engine" run -d --name "$name" --read-only --cap-drop=ALL --security-opt=no-new-privileges \
    --pids-limit=128 --memory=512m \
    --tmpfs /tmp:rw,noexec,nosuid,nodev,size=300m \
    --tmpfs /data:rw,noexec,nosuid,nodev,size=64m,uid=10001,gid=10001 \
    -p 127.0.0.1::8082 -v "$work/config.yaml:/config/config.yaml:ro" "$image"
address=$("$engine" port "$name" 8082/tcp | head -1)
attempt=0
while [ "$attempt" -lt 45 ]; do
    if curl --fail --silent --max-time 2 "http://$address/health"; then
        curl --fail --silent --max-time 5 "http://$address/metrics" > "$work/metrics"
        test -s "$work/metrics"
        "$engine" image inspect "$image" --format '{{.Size}}'
        exit 0
    fi
    attempt=$((attempt + 1))
    sleep 1
done
"$engine" logs "$name"
exit 1
