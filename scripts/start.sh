#!/usr/bin/env bash
# start.sh — build and launch an N-node Raft cluster
# Usage: ./scripts/start.sh [nodes]   (default: 3, must be odd)
set -e

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$REPO_ROOT/dist/kvnode"
DATA="$REPO_ROOT/data"
N="${1:-3}"
BASE_PORT=8001

if (( N % 2 == 0 )); then
  echo "error: node count must be odd (3, 5, 7, …)" >&2
  exit 1
fi
if (( N < 1 )); then
  echo "error: need at least 1 node" >&2
  exit 1
fi

echo "==> Building..."
cd "$REPO_ROOT"
go build -o "$BIN" ./cmd/kvnode

echo "==> Starting ${N}-node cluster..."
mkdir -p "$DATA"

# Build address list
ADDRS=()
for (( i=1; i<=N; i++ )); do
  ADDRS+=("localhost:$(( BASE_PORT + i - 1 ))")
done

# Launch each node with all other nodes as peers
for (( i=1; i<=N; i++ )); do
  PORT=$(( BASE_PORT + i - 1 ))
  PEERS=""
  for (( j=1; j<=N; j++ )); do
    if (( j != i )); then
      [[ -n "$PEERS" ]] && PEERS+=","
      PEERS+="localhost:$(( BASE_PORT + j - 1 ))"
    fi
  done
  "$BIN" -id "$i" -addr ":${PORT}" -peers "$PEERS" -data "$DATA" &
done

echo ""
echo "  Cluster is running (${N} nodes)."
echo ""
echo "  Open the dashboard:"
echo "    file://$REPO_ROOT/web/index.html"
echo ""
echo "  Nodes:"
for (( i=1; i<=N; i++ )); do
  echo "    http://localhost:$(( BASE_PORT + i - 1 ))   (Node ${i})"
done
echo ""
echo "  Wait ~1 second for leader election."
echo "  Run ./scripts/stop.sh to shut down."
echo ""

echo "window.CLUSTER_NODES = ${N};" > "$REPO_ROOT/web/cluster-config.js"

if command -v open &>/dev/null; then
  sleep 1
  open "$REPO_ROOT/web/index.html"
fi
