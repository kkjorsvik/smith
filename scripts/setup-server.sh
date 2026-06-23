#!/usr/bin/env bash
#
# smith control-plane setup — run on the server box with the smith-server binary
# present next to this script (or pass its path as $1):
#
#   sudo ./setup-server.sh [path/to/smith-server]
#
# Installs containerd, lays out /etc/smith and /var/lib/smith, generates the API
# token, bootstraps the CA + server cert, and starts smith-server as a service.
# Idempotent: re-running preserves the token and CA (gencerts reuses them).
set -euo pipefail

SCRIPT_DIR="$(dirname "$(readlink -f "$0")")"
SERVER_BIN="${1:-}"
if [[ -z "$SERVER_BIN" ]]; then
  for cand in "${SCRIPT_DIR}/smith-server" "${SCRIPT_DIR}/../bin/smith-server"; do
    [[ -x "$cand" ]] && SERVER_BIN="$cand" && break
  done
fi

CERT_DIR="/etc/smith/certs"
BIN_DST="/usr/local/bin/smith-server"
TOKEN="/etc/smith/token"
UNIT_DST="/etc/systemd/system/smith-server.service"

if [[ $EUID -ne 0 ]]; then
  echo "setup: must run as root (try: sudo ./setup-server.sh)" >&2
  exit 1
fi
if [[ -z "$SERVER_BIN" || ! -x "$SERVER_BIN" ]]; then
  echo "setup: smith-server binary not found — pass its path: sudo ./setup-server.sh path/to/smith-server" >&2
  exit 1
fi

echo "==> Installing containerd"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq containerd ca-certificates openssl
systemctl enable --now containerd

echo "==> Creating directories"
mkdir -p "$CERT_DIR" /var/lib/smith
chmod 700 /etc/smith "$CERT_DIR"

echo "==> Installing smith-server binary -> ${BIN_DST}"
install -m 0755 "$SERVER_BIN" "$BIN_DST"

if [[ ! -s "$TOKEN" ]]; then
  echo "==> Generating API token -> ${TOKEN}"
  openssl rand -hex 32 > "$TOKEN"
  chmod 600 "$TOKEN"
else
  echo "==> API token already present, keeping it"
fi

if [[ ! -f "${CERT_DIR}/ca.crt" ]]; then
  echo "==> Bootstrapping CA + server cert (gencerts)"
  "$BIN_DST" gencerts -out "$CERT_DIR"
else
  echo "==> CA already present, keeping it"
fi

echo "==> Installing systemd unit -> ${UNIT_DST}"
install -m 0644 "${SCRIPT_DIR}/smith-server.service" "$UNIT_DST"
systemctl daemon-reload
systemctl enable --now smith-server.service

cat <<EOF

Done. smith-server is installed and enabled.
  status: systemctl status smith-server
  logs:   journalctl -u smith-server -f

Next steps:
  * The public :443 cert needs AWS creds with Route 53 access. Provide them via
    /etc/smith/server.env (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / region),
    an instance role, or ~/.aws — then: systemctl restart smith-server
  * Add an agent (run here, where the CA lives):
      smith-server add-agent -host smith-agent-03.kkjorsvik.com -binary bin/smith-agent
    then scp the resulting tarball to the new box and run ./setup.sh there.
EOF
