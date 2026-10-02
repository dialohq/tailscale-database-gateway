#!/usr/bin/env bash
set -euo pipefail
project_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
pick_port() {
  python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()'
}

test_dir="$(mktemp -d "${TMPDIR:-/tmp}/clickhouse-gateway.XXXXXX")"
server_pid=""
cleanup() {
  if [ -n "$server_pid" ]; then
    kill "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
  fi
  rm -rf "$test_dir"
}
trap cleanup EXIT INT TERM

native_port="$(pick_port)"
http_port="$(pick_port)"
while [ "$http_port" = "$native_port" ]; do
  http_port="$(pick_port)"
done
mkdir -p \
  "$test_dir/data" \
  "$test_dir/tmp" \
  "$test_dir/user_files" \
  "$test_dir/format_schemas"
cat > "$test_dir/config.xml" <<EOF
<clickhouse>
  <logger><level>warning</level><console>1</console></logger>
  <listen_host>127.0.0.1</listen_host>
  <tcp_port>$native_port</tcp_port>
  <http_port>$http_port</http_port>
  <path>$test_dir/data/</path>
  <tmp_path>$test_dir/tmp/</tmp_path>
  <user_files_path>$test_dir/user_files/</user_files_path>
  <format_schema_path>$test_dir/format_schemas/</format_schema_path>
  <profiles><default/></profiles>
  <quotas><default/></quotas>
  <users>
    <vault_user>
      <password>fresh-password</password>
      <networks><ip>127.0.0.1</ip></networks>
      <profile>default</profile>
      <quota>default</quota>
    </vault_user>
  </users>
</clickhouse>
EOF

clickhouse server --config-file="$test_dir/config.xml" > "$test_dir/server.log" 2>&1 &
server_pid=$!
ready=""
for _ in $(seq 1 100); do
  if clickhouse client \
    --host 127.0.0.1 \
    --port "$native_port" \
    --user vault_user \
    --password fresh-password \
    --query "SELECT 1" >/dev/null 2>&1; then
    ready=1
    break
  fi
  if ! kill -0 "$server_pid" 2>/dev/null; then
    break
  fi
  sleep 0.1
done
if [ -z "$ready" ]; then
  cat "$test_dir/server.log" >&2
  exit 1
fi

export CLICKHOUSE_BINARY="$(command -v clickhouse)"
export CLICKHOUSE_TEST_NATIVE_UPSTREAM="127.0.0.1:$native_port"
export CLICKHOUSE_TEST_HTTP_UPSTREAM="http://127.0.0.1:$http_port"
cd "$project_dir"
go test -tags=integration ./internal/clickhouse -run '^TestClickHouseGatewaysEndToEnd$' -count=1 "$@"
