#!/usr/bin/env bash
#
# Provision a database + login role on the shared smith Postgres for a new app.
# One Postgres engine serves many services; this adds <app>'s database and user
# WITHOUT touching the postgres workload (no restart, no downtime). Idempotent:
# re-running leaves an existing db/role as-is (it does NOT rotate the password).
#
#   ./scripts/provision-db.sh forgejo             # generates a password
#   ./scripts/provision-db.sh woodpecker s3cret   # use a supplied password
#
# Connects as the Postgres superuser via the postgres service's NodePort on a
# cluster node. Requires psql + jq. Superuser password comes from
# $SMITH_PG_SUPERPASS, else you're prompted. Overrides:
#   SMITH_SERVER_HOST  control-plane host (default smith-server-01.kkjorsvik.com)
#   SMITH_PG_SERVICE   service name of the shared Postgres (default "postgres")
#   SMITH_PG_HOST      node to reach the NodePort on (default: first node)
#   SMITH_TOKEN        API bearer token (default: sudo cat /etc/smith/token)
set -euo pipefail

SERVER_HOST="${SMITH_SERVER_HOST:-smith-server-01.kkjorsvik.com}"
PG_SERVICE="${SMITH_PG_SERVICE:-postgres}"

app="${1:-}"
app_pass="${2:-}"
if [[ -z "$app" ]]; then
  echo "usage: $0 <app> [password]" >&2
  exit 1
fi
if ! [[ "$app" =~ ^[a-z][a-z0-9_]*$ ]]; then
  echo "provision-db: app name must be a valid identifier [a-z][a-z0-9_]* (got '$app')" >&2
  exit 1
fi

command -v psql >/dev/null || { echo "provision-db: psql required (sudo apt install postgresql-client)" >&2; exit 1; }
command -v jq   >/dev/null || { echo "provision-db: jq required" >&2; exit 1; }

TOKEN="${SMITH_TOKEN:-$(sudo cat /etc/smith/token)}"
api() { curl -fsS -H "Authorization: Bearer ${TOKEN}" "https://${SERVER_HOST}$1" 2>/dev/null; }

# The postgres service: ClusterIP is what apps use; NodePort is for this admin call.
svc="$(api /services | jq -c --arg n "$PG_SERVICE" '.[] | select(.name==$n)' || true)"
if [[ -z "$svc" ]]; then
  echo "provision-db: no '$PG_SERVICE' service found (or API unreachable) — create the service first" >&2
  exit 1
fi
nodeport="$(jq -r '.node_port' <<<"$svc")"
clusterip="$(jq -r '.cluster_ip' <<<"$svc")"

pghost="${SMITH_PG_HOST:-$(api /nodes | jq -r '.[0].addr' | cut -d: -f1)}"
if [[ -z "$pghost" || "$pghost" == "null" ]]; then
  echo "provision-db: could not determine a node host (set SMITH_PG_HOST)" >&2
  exit 1
fi

if [[ -z "${SMITH_PG_SUPERPASS:-}" ]]; then
  read -rs -p "Postgres superuser (postgres) password: " SMITH_PG_SUPERPASS; echo
fi

generated=false
if [[ -z "$app_pass" ]]; then
  app_pass="$(openssl rand -hex 16)"
  generated=true
fi
# Escape single quotes so the password is safe inside the SQL string literal.
app_pass_sql="${app_pass//\'/\'\'}"

echo "==> Provisioning database '$app' on '${PG_SERVICE}' via ${pghost}:${nodeport}"
PGPASSWORD="$SMITH_PG_SUPERPASS" psql -v ON_ERROR_STOP=1 \
  -h "$pghost" -p "$nodeport" -U postgres -d postgres <<SQL
SELECT 'CREATE DATABASE "$app"'
 WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = '$app')\gexec

DO \$\$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '$app') THEN
    CREATE ROLE "$app" LOGIN PASSWORD '$app_pass_sql';
  END IF;
END
\$\$;

GRANT ALL PRIVILEGES ON DATABASE "$app" TO "$app";
\connect "$app"
GRANT ALL ON SCHEMA public TO "$app";
ALTER SCHEMA public OWNER TO "$app";
SQL

echo
echo "Database '$app' ready. App connection settings:"
echo "    host:     ${clusterip}:5432    (stable ClusterIP)"
echo "    database: $app"
echo "    user:     $app"
if $generated; then
  echo "    password: $app_pass    <-- generated; save it"
else
  echo "    password: (the one you supplied)"
fi
