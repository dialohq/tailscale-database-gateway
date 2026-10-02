#!/usr/bin/env bash
set -euo pipefail
project_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
pick_port() {
  python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()'
}

if [ "$(id -u)" = 0 ]; then
  echo "PostgreSQL initdb refuses to run as root" >&2
  exit 1
fi

test_dir="$(mktemp -d "${TMPDIR:-/tmp}/postgres-gateway.XXXXXX")"
data_dir="$test_dir/data"
socket_dir="$test_dir/socket"
log_file="$test_dir/postgres.log"
server_started=""
cleanup() {
  if [ -n "$server_started" ]; then
    pg_ctl -D "$data_dir" -m fast -w stop >/dev/null 2>&1 || cat "$log_file" >&2
  fi
  rm -rf "$test_dir"
}
trap cleanup EXIT INT TERM

port="$(pick_port)"
mkdir -p "$socket_dir"
printf '%s\n' superuser-password > "$test_dir/superuser-password"
initdb \
  -D "$data_dir" \
  --username=postgres \
  --no-locale \
  -E UTF8 \
  --auth-host=scram-sha-256 \
  --pwfile="$test_dir/superuser-password" >/dev/null
pg_ctl \
  -D "$data_dir" \
  -l "$log_file" \
  -o "-h 127.0.0.1 -p $port -k $socket_dir" \
  -w start >/dev/null
server_started=1

admin_url="postgresql://postgres@127.0.0.1:$port/postgres?sslmode=disable"
database_url="postgresql://postgres@127.0.0.1:$port/gateway_db?sslmode=disable"
PGPASSWORD=superuser-password psql "$admin_url" -v ON_ERROR_STOP=1 -c 'CREATE ROLE gateway_readonly' >/dev/null
PGPASSWORD=superuser-password psql "$admin_url" -v ON_ERROR_STOP=1 -c "CREATE ROLE gateway_login LOGIN PASSWORD 'initial-password' IN ROLE gateway_readonly" >/dev/null
PGPASSWORD=superuser-password psql "$admin_url" -v ON_ERROR_STOP=1 -c 'CREATE DATABASE gateway_db' >/dev/null
PGPASSWORD=superuser-password psql "$database_url" -v ON_ERROR_STOP=1 -c "CREATE TABLE protected (value text); INSERT INTO protected VALUES ('visible'); GRANT USAGE ON SCHEMA public TO gateway_readonly; GRANT SELECT ON protected TO gateway_readonly" >/dev/null

export POSTGRES_TEST_UPSTREAM="postgresql://127.0.0.1:$port/postgres?sslmode=disable"
export POSTGRES_TEST_ADMIN="postgresql://postgres:superuser-password@127.0.0.1:$port/postgres?sslmode=disable"
cd "$project_dir"
go test -tags=integration ./internal/postgres -run '^TestPostgresGatewayEndToEnd$' -count=1 -v "$@"
