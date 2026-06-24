#!/usr/bin/env bash
#
# smith agent setup — run from inside the unpacked bundle directory:
#
#   tar xzf smith-agent-03.tar.gz
#   cd smith-agent-03
#   sudo ./setup.sh
#
# Installs containerd + CNI plugins, places this agent's certs/binary/unit, and
# starts smith-agent as a systemd service. Idempotent: safe to re-run.
set -euo pipefail

# Pinned CNI plugins release (bridge/host-local/portmap live here).
CNI_PLUGINS_VERSION="v1.6.2"

CERT_DIR="/etc/smith/certs"
BIN_DST="/usr/local/bin/smith-agent"
ENV_DST="/etc/smith/agent.env"
UNIT_DST="/etc/systemd/system/smith-agent.service"

if [[ $EUID -ne 0 ]]; then
  echo "setup: must run as root (try: sudo ./setup.sh)" >&2
  exit 1
fi

# Run from the bundle directory regardless of cwd.
cd "$(dirname "$(readlink -f "$0")")"

if [[ ! -f agent.env ]]; then
  echo "setup: agent.env not found — run this from the unpacked bundle dir" >&2
  exit 1
fi
# shellcheck disable=SC1091
source ./agent.env

ca_file="$(basename "${SMITH_CA}")"
cert_file="$(basename "${SMITH_CERT}")"
key_file="$(basename "${SMITH_KEY}")"

echo "==> Installing containerd + NFS client"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
# nfs-common provides mount.nfs, needed for stateful workload volumes.
apt-get install -y -qq containerd ca-certificates curl tar nfs-common
systemctl enable --now containerd

echo "==> Installing CNI plugins ${CNI_PLUGINS_VERSION} -> /opt/cni/bin"
arch="$(dpkg --print-architecture)" # amd64 | arm64
case "$arch" in
  amd64|arm64) ;;
  *) echo "setup: unsupported arch '$arch'" >&2; exit 1 ;;
esac
if [[ ! -x /opt/cni/bin/bridge ]]; then
  mkdir -p /opt/cni/bin
  tgz="cni-plugins-linux-${arch}-${CNI_PLUGINS_VERSION}.tgz"
  url="https://github.com/containernetworking/plugins/releases/download/${CNI_PLUGINS_VERSION}/${tgz}"
  tmp="$(mktemp -d)"
  curl -fsSL "$url" -o "${tmp}/${tgz}"
  tar -xzf "${tmp}/${tgz}" -C /opt/cni/bin
  rm -rf "$tmp"
else
  echo "    /opt/cni/bin/bridge already present, skipping"
fi

echo "==> Placing certs in ${CERT_DIR}"
mkdir -p "$CERT_DIR"
chmod 700 "$CERT_DIR"
install -m 0644 "./${ca_file}"   "${CERT_DIR}/${ca_file}"
install -m 0644 "./${cert_file}" "${CERT_DIR}/${cert_file}"
install -m 0600 "./${key_file}"  "${CERT_DIR}/${key_file}"

echo "==> Installing smith-agent binary -> ${BIN_DST}"
install -m 0755 ./smith-agent "$BIN_DST"

echo "==> Installing env file -> ${ENV_DST}"
install -m 0600 ./agent.env "$ENV_DST"

echo "==> Installing systemd unit -> ${UNIT_DST}"
install -m 0644 ./smith-agent.service "$UNIT_DST"
systemctl daemon-reload
systemctl enable --now smith-agent.service

echo
echo "Done. smith-agent (${SMITH_ID}) is running."
echo "  status: systemctl status smith-agent"
echo "  logs:   journalctl -u smith-agent -f"
