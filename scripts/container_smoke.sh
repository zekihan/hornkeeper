#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
image="hornkeeper-smoke:local"
docker build --build-arg VERSION=smoke -t "$image" .
test "$(docker run --rm --network none --read-only --cap-drop ALL --security-opt no-new-privileges "$image" --version)" = smoke
test "$(docker image inspect --format '{{.Config.User}}' "$image")" = 25000:25000
# Startup validation must fail before attempting Kubernetes access.
status=0
output=$(docker run --rm --network none --read-only --cap-drop ALL --security-opt no-new-privileges "$image" --default-replicas=0 2>&1) || status=$?
if [ "$status" -ne 1 ] || ! printf '%s\n' "$output" | grep -Fq 'replicas must be between 1 and 20, got 0'; then
  printf 'expected replica validation error (exit 1), got exit %s\n%s\n' "$status" "$output" >&2
  exit 1
fi
