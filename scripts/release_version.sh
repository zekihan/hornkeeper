#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
version=$(cat VERSION)
if ! printf '%s\n' "$version" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$'; then
  echo 'VERSION must be a container-compatible semantic version' >&2
  exit 1
fi
case "${GITHUB_REF:-}" in
  refs/tags/v*)
    if [ "${GITHUB_REF#refs/tags/v}" != "$version" ]; then
      echo 'Release tag must match VERSION' >&2
      exit 1
    fi
    ;;
esac
printf '%s\n' "$version"
