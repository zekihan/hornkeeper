#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
image="hornkeeper-smoke:local"
docker build --build-arg VERSION=smoke -t "$image" .
test "$(docker run --rm --network none --read-only --cap-drop ALL --security-opt no-new-privileges "$image" --version)" = smoke
test "$(docker image inspect --format '{{.Config.User}}' "$image")" = 25000:25000
# Startup validation must fail before attempting Kubernetes access.
if docker run --rm --network none --read-only --cap-drop ALL --security-opt no-new-privileges "$image" --default-replicas=0; then
  echo 'invalid default replicas unexpectedly accepted' >&2
  exit 1
fi
