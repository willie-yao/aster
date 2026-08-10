#!/usr/bin/env bash
set -euo pipefail

image=${1:?usage: test-agent-sandbox-critic-image.sh IMAGE}
output=$(docker run --rm "$image" 2>&1 || true)
printf '%s' "$output" | grep -Fq 'PROW_AI_CAUSAL_CRITIC_REQUEST_B64 is required'
printf '%s' "$output" | grep -Fq '"terminal_state":"failed"'
if printf '%s' "$output" | grep -Eiq 'authorization|api[-_]?key|bearer'; then
  echo "critic image emitted credential-like output" >&2
  exit 1
fi
