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
# Run it as your normal user (it sudo's the privileged local steps); running it
# under sudo also works. Either way ssh/scp use your keys. Requires that your
# user can sudo non-interactively (NOPASSWD) on the agents, since the remote
# binary swap runs `sudo` over a non-interactive ssh.
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

# Work whether started as your user (sudo'ing the privileged steps) or under
# sudo. Local privileged steps run as root; ssh/scp always run as the invoking
# user so they use *your* keys — root on this box has none for the agents.
if [[ $EUID -eq 0 ]]; then
  RUN_USER="${SUDO_USER:-root}"
  if [[ "$RUN_USER" == "root" ]]; then
    echo "update: run as your normal user (with sudo access), not as root —" >&2
    echo "        ssh to the agents needs your SSH keys, which root lacks." >&2
    exit 1
  fi
  asroot() { "$@"; }
  asuser() { sudo -u "$RUN_USER" -H "$@"; }
else
  asroot() { sudo "$@"; }
  asuser() { "$@"; }
fi

TOKEN="$(asroot cat /etc/smith/token)"
api() { curl -fsS -H "Authorization: Bearer ${TOKEN}" "https://${SERVER_HOST}$1" 2>/dev/null; }
node_id()   { echo "${1%%.*}"; }   # smith-agent-01.kkjorsvik.com -> smith-agent-01

# total running containers across the whole cluster. A replica bounced during a
# roll may reschedule to a DIFFERENT node and stay there (sticky), so cluster-wide
# total — not per-node count — is the right measure of recovery.
cluster_running_total() { api /status 2>/dev/null | jq '[.[] | .[]? | select(.status=="running")] | length' 2>/dev/null || echo 0; }

# predicates (called directly by poll, so they stay in function scope)
node_present()    { api /nodes 2>/dev/null | jq -e --arg n "$1" 'any(.[]; .id==$n)' >/dev/null 2>&1; }
total_restored()  { (( "$(cluster_running_total)" >= "$1" )); }

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

# Record the cluster-wide running total BEFORE any disruption. Each agent roll
# must bring the cluster back to this many running replicas — wherever they land.
TOTAL_BEFORE="$(cluster_running_total)"
echo "==> ${TOTAL_BEFORE} running replicas cluster-wide before update"

echo "==> Building binaries in ${REPO_DIR}"
( cd "$REPO_DIR" && go build -o bin/smith-server ./cmd/server && go build -o bin/smith-agent ./cmd/agent )

echo "==> Updating control plane"
asroot install -m0755 "${REPO_DIR}/bin/smith-server" /usr/local/bin/smith-server
asroot systemctl restart smith-server
if ! poll "control plane API" 60 api /nodes >/dev/null; then
  echo "  control plane did not come back — check journalctl -u smith-server" >&2
  exit 1
fi
echo "    control plane back up"

# We do NOT wait for the running agents to re-register here: they may be on an
# older binary that can't self-heal a 404. Each agent is restarted below, which
# re-registers it at startup regardless — the roll itself brings every node back.
for h in "${AGENTS[@]}"; do
  nid="$(node_id "$h")"
  echo "==> Rolling ${nid} (${h})"

  asuser scp -q "${REPO_DIR}/bin/smith-agent" "${h}:~/smith-agent.new"
  asuser ssh "$h" 'sudo install -m0755 ~/smith-agent.new /usr/local/bin/smith-agent && sudo systemctl restart smith-agent && rm -f ~/smith-agent.new'

  # Re-registration happens at agent startup, so this should always succeed.
  # If it doesn't, bail rather than draining more capacity by rolling the next.
  if ! poll "${nid} to re-register" 90 node_present "$nid"; then
    echo "  ${nid} did not re-register after restart — aborting before rolling more agents" >&2
    echo "  check: journalctl -u smith-agent on ${h}" >&2
    exit 1
  fi

  # Wait for the cluster to return to its pre-roll running total before touching
  # the next node. Replicas bounced off this node may come back here OR move to
  # another node and stay (sticky) — either way the cluster total recovers, so we
  # check that, not this node's own count.
  if poll "cluster to return to ${TOTAL_BEFORE} running replicas" 180 total_restored "$TOTAL_BEFORE"; then
    echo "    cluster healthy (${TOTAL_BEFORE} running)"
  else
    echo "    WARNING: cluster below ${TOTAL_BEFORE} running replicas — check the agents before continuing" >&2
  fi
done

echo "==> Done. Cluster updated."
api /nodes | jq -r '.[] | "  \(.id)\t\(.addr)"'
