#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
output="$(make -C "$repo_root" -n daemon)"

if ! grep -Eq 'go build .* -o bin/multica ./cmd/multica' <<<"$output"; then
  echo "make daemon must build server/bin/multica" >&2
  exit 1
fi

if ! grep -Fq 'server/bin/multica daemon restart --profile local' <<<"$output"; then
  echo "make daemon must launch the stable server/bin/multica binary" >&2
  exit 1
fi

if grep -Eq 'go run .*\./cmd/multica' <<<"$output"; then
  echo "make daemon must not launch the daemon through go run" >&2
  exit 1
fi
