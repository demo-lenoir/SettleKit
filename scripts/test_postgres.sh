#!/usr/bin/env bash
set -euo pipefail

if [[ -n "${SETTLEKIT_TEST_DATABASE_URL:-}" ]]; then
  tmp_dir="$(mktemp -d)"
  base_url="$SETTLEKIT_TEST_DATABASE_URL"
  read -r test_schema test_url < <(go run scripts/testdb.go create)
  export SETTLEKIT_TEST_DATABASE_URL="$test_url"
  cleanup_external() {
    SETTLEKIT_TEST_DATABASE_URL="$base_url" go run scripts/testdb.go drop "$test_schema" >/dev/null || true
    find "$tmp_dir" -depth -delete
  }
  trap cleanup_external EXIT
elif [[ -n "${SETTLEKIT_PG_BIN:-}" ]]; then
  pg_bin="$SETTLEKIT_PG_BIN"
elif command -v pg_config >/dev/null 2>&1; then
  pg_bin="$(pg_config --bindir)"
elif command -v brew >/dev/null 2>&1; then
  pg_bin="$(brew --prefix postgresql@18)/bin"
else
  echo "PostgreSQL 18 binaries not found; set SETTLEKIT_PG_BIN" >&2
  exit 1
fi

if [[ -z "${SETTLEKIT_TEST_DATABASE_URL:-}" ]]; then
  tmp_dir="$(mktemp -d)"
  data_dir="$tmp_dir/data"
  socket_dir="$tmp_dir/socket"
  mkdir "$socket_dir"
  cleanup() {
    "$pg_bin/pg_ctl" -D "$data_dir" -m fast stop >/dev/null 2>&1 || true
    find "$tmp_dir" -depth -delete
  }
  trap cleanup EXIT
  "$pg_bin/initdb" -D "$data_dir" -U settlekit --auth-local=trust --auth-host=reject --no-instructions >/dev/null
  "$pg_bin/pg_ctl" -D "$data_dir" -l "$tmp_dir/postgres.log" -o "-k $socket_dir -h '' -p 55439" start >/dev/null
  export SETTLEKIT_TEST_DATABASE_URL="host=$socket_dir port=55439 user=settlekit dbname=postgres sslmode=disable"
fi
if [[ "${1:-}" == "--anvil" ]]; then
  go build -o "$tmp_dir/settlekit" ./cmd/settlekit
  export SETTLEKIT_TEST_SERVICE_BINARY="$tmp_dir/settlekit"
  bash scripts/test_anvil.sh
  exit
fi
go test -p 1 ./internal/store ./internal/api ./internal/indexer ./internal/webhook ./internal/telemetry -count=1 -v
go test -p 1 -race ./internal/store ./internal/api ./internal/indexer ./internal/webhook ./internal/telemetry -count=1 -v
