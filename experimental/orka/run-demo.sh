#!/usr/bin/env bash
# Orka dashboard demo: run the full pipeline for one consumer end to end and
# produce a renderable dashboard data dir, using the Orka stack on the
# `orka-spike` kind cluster.
#
#   fetcher (-ai=false)  -> dashboard skeleton (jobs/*.json)
#   orka-producer        -> content-addressed Tasks + per-build header-routed Tools
#   kubectl apply        -> Orka runs the analyses (Copilot + GCS tools)
#   orka-ingestor        -> patches ai_summary/ai_analysis back into jobs/*.json
#
# Prereqs: the Orka stack deployed on kind (see experimental/orka/README.md):
# Orka + CRDs, the copilot Provider + proxy, and the artifact-tool Deployment.
#
# Usage: experimental/orka/run-demo.sh <consumer-project-dir>
#   env: BUILDS (default 3), VERSION (default v1), CTX (default kind-orka-spike),
#        NS (default orka-system), OUT (default a temp dir)
set -euo pipefail

CONSUMER="${1:?usage: run-demo.sh <consumer-project-dir>}"
CONSUMER="$(cd "$CONSUMER" && pwd)"
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CTX="${CTX:-kind-orka-spike}"
NS="${NS:-orka-system}"
BUILDS="${BUILDS:-3}"
VERSION="${VERSION:-v1}"
WORK="${OUT:-$(mktemp -d)}"
DATA="$WORK/data"; TASKS="$WORK/tasks"; TOOLS="$WORK/build-tools"
BIN="$WORK/bin"

echo "==> work dir: $WORK"
mkdir -p "$BIN"

echo "==> building fetcher, producer, ingestor"
( cd "$REPO/backend"
  go build -o "$BIN/fetcher" ./cmd/fetcher
  go build -o "$BIN/producer" ./cmd/orka-producer
  go build -o "$BIN/ingestor" ./cmd/orka-ingestor )

echo "==> M1: dashboard skeleton (no AI), $BUILDS builds/job"
"$BIN/fetcher" -project-dir="$CONSUMER" -out="$DATA" -builds="$BUILDS"

echo "==> M2: producing Tasks + per-build Tools"
"$BIN/producer" -data="$DATA" -project-dir="$CONSUMER" \
  -tool-manifests="$REPO/experimental/orka/manifests" \
  -tasks-out="$TASKS" -tools-out="$TOOLS" -namespace="$NS" -version="$VERSION"

ntasks=$(find "$TASKS" -name '*.yaml' | wc -l | tr -d ' ')
if [ "$ntasks" = "0" ]; then
  echo "==> no failing tests in this window; dashboard skeleton is at $DATA"
  exit 0
fi

echo "==> applying $ntasks Tasks and their per-build Tools"
kubectl --context "$CTX" apply -f "$TOOLS" >/dev/null
kubectl --context "$CTX" apply -f "$TASKS" >/dev/null

echo "==> waiting for Tasks to settle"
for _ in $(seq 1 80); do
  pending=$(kubectl --context "$CTX" get tasks -n "$NS" -l app.kubernetes.io/managed-by=orka-producer \
    -o jsonpath='{range .items[*]}{.status.phase}{"\n"}{end}' 2>/dev/null \
    | grep -vcE 'Succeeded|Failed' || true)
  echo "    tasks not yet terminal: $pending"
  [ "$pending" = "0" ] && break
  sleep 15
done

echo "==> M3: ingesting results into the dashboard"
kubectl --context "$CTX" port-forward -n "$NS" svc/orka 18099:8080 >/dev/null 2>&1 &
PF=$!
trap 'kill $PF 2>/dev/null || true' EXIT
sleep 3
TOK="$(kubectl --context "$CTX" create token orka -n "$NS" --duration=20m)"
"$BIN/ingestor" -data="$DATA" -api="http://localhost:18099" -token="$TOK" -version="$VERSION"

echo
echo "==> done. Orka-produced dashboard data is at: $DATA"
echo "    render it locally:  cp -r $DATA/* $REPO/frontend/public/data/ && (cd $REPO/frontend && npm run dev)"
