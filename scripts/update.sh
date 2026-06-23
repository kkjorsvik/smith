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

# Record baselines BEFORE any disruption, while the old agents are still
# registered, so the per-agent recovery check compares against real counts.
# Best-effort: a node not currently registered reads as 0.
declare -A BEFORE
echo "==> Recording current replica counts"
for h in "${AGENTS[@]}"; do
  nid="$(node_id "$h")"
  BEFORE[$nid]="$(running_count "$nid")"
  echo "    ${nid}: ${BEFORE[$nid]} running"
done

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
  echo "==> Rolling ${nid} (${h}) — ${BEFORE[$nid]} running replicas before update"

  asuser scp -q "${REPO_DIR}/bin/smith-agent" "${h}:~/smith-agent.new"
  asuser ssh "$h" 'sudo install -m0755 ~/smith-agent.new /usr/local/bin/smith-agent && sudo systemctl restart smith-agent && rm -f ~/smith-agent.new'

  # Re-registration happens at agent startup, so this should always succeed.
  # If it doesn't, bail rather than draining more capacity by rolling the next.
  if ! poll "${nid} to re-register" 90 node_present "$nid"; then
    echo "  ${nid} did not re-register after restart — aborting before rolling more agents" >&2
    echo "  check: journalctl -u smith-agent on ${h}" >&2
    exit 1
  fi

  # Replica recovery is best-effort: a quick restart keeps assignments sticky to
  # this node, but a slow one (>30s dead threshold) may move them elsewhere.
  if poll "${nid} replicas to return to running (>= ${BEFORE[$nid]})" 180 replicas_restored "$nid" "${BEFORE[$nid]}"; then
    echo "    ${nid} healthy"
  else
    echo "    WARNING: ${nid} did not return to ${BEFORE[$nid]} running replicas — check journalctl -u smith-agent on ${h}" >&2
  fi
done

echo "==> Done. Cluster updated."
api /nodes | jq -r '.[] | "  \(.id)\t\(.addr)"'
