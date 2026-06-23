#!/usr/bin/env bash
#
# Rolling update for a running smith cluster. Run on the control-plane box
# (where the repo, binaries, token, and ssh access to the agents live):
#
#   ./scripts/update.sh smith-agent-01.kkjorsvik.com smith-agent-02.kkjorsvik.com ...
#
# Builds both binaries, updates + restarts the control plane, then rolls each
# agent one at a time: pushes the new binary, restarts the service, and waits
# for the node to re-register and its replicas to return to running before
# moving on. Each agent's containers briefly cycle as it restarts; services
# stay up as long as a workload's replicas are spread across nodes.
#
# Builds from the current checkout — `git pull` first if you want newer code.
# Uses sudo for local privileged steps and ssh (as you) + remote sudo on agents.
set -euo pipefail

SERVER_HOST="${SMITH_SERVER_HOST:-smith-server-01.kkjorsvik.com}"
REPO_DIR="$(cd "$(dirname "$(readlink -f "$0")")/.." && pwd)"

if [[ $# -eq 0 ]]; then
  echo "usage: $0 <agent-host> [agent-host ...]" >&2
  echo "  e.g. $0 smith-agent-01.kkjorsvik.com smith-agent-02.kkjorsvik.com" >&2
  exit 1
fi
AGENTS=("$@")

command -v jq >/dev/null || { echo "update: jq is required" >&2; exit 1; }

TOKEN="$(sudo cat /etc/smith/token)"
api()       { curl -fsS -H "Authorization: Bearer ${TOKEN}" "https://${SERVER_HOST}$1"; }
node_id()   { echo "${1%%.*}"; }   # smith-agent-01.kkjorsvik.com -> smith-agent-01

# count of running containers the cluster reports on a node
running_count() { api /status 2>/dev/null | jq --arg n "$1" '[.[$n][]? | select(.status=="running")] | length' 2>/dev/null || echo 0; }

# predicates (called directly by poll, so they stay in function scope)
node_present()      { api /nodes 2>/dev/null | jq -e --arg n "$1" 'any(.[]; .id==$n)' >/dev/null 2>&1; }
replicas_restored() { (( "$(running_count "$1")" >= "$2" )); }

# poll <desc> <timeout_s> <predicate> [args...]
poll() {
  local desc="$1" timeout="$2"; shift 2
  local waited=0
  until "$@"; do
    if (( waited >= timeout )); then
      echo "  timed out after ${timeout}s waiting for ${desc}" >&2
      return 1
    fi
    sleep 5; waited=$((waited+5))
  done
}

echo "==> Building binaries in ${REPO_DIR}"
( cd "$REPO_DIR" && go build -o bin/smith-server ./cmd/server && go build -o bin/smith-agent ./cmd/agent )

echo "==> Updating control plane"
sudo install -m0755 "${REPO_DIR}/bin/smith-server" /usr/local/bin/smith-server
sudo systemctl restart smith-server
poll "control plane API" 60 api /nodes >/dev/null
echo "    control plane back up"

echo "==> Waiting for agents to re-register with the restarted control plane"
for h in "${AGENTS[@]}"; do
  poll "$(node_id "$h") to re-register" 60 node_present "$(node_id "$h")"
done

for h in "${AGENTS[@]}"; do
  nid="$(node_id "$h")"
  before="$(running_count "$nid")"
  echo "==> Rolling ${nid} (${h}) — ${before} running replicas before restart"

  scp -q "${REPO_DIR}/bin/smith-agent" "${h}:~/smith-agent.new"
  ssh "$h" 'sudo install -m0755 ~/smith-agent.new /usr/local/bin/smith-agent && sudo systemctl restart smith-agent && rm -f ~/smith-agent.new'

  # Re-registration is required before rolling the next agent — bail if a node
  # doesn't come back, rather than draining more capacity.
  poll "${nid} to re-register" 60 node_present "$nid"

  # Replica recovery is best-effort: a quick restart keeps assignments sticky to
  # this node, but a slow one (>30s dead threshold) may move them elsewhere.
  if poll "${nid} replicas to return to running (>= ${before})" 180 replicas_restored "$nid" "$before"; then
    echo "    ${nid} healthy"
  else
    echo "    WARNING: ${nid} did not return to ${before} running replicas — check journalctl -u smith-agent on ${h}" >&2
  fi
done

echo "==> Done. Cluster updated."
api /nodes | jq -r '.[] | "  \(.id)\t\(.addr)"'
