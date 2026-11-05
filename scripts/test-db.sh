#!/usr/bin/env bash
set -euo pipefail
umask 077

# Never attach to a supplied server, even a database with an apparently safe name.
for option in PG_TEST_SOCKET PG_TEST_PORT PG_TEST_DATABASE PG_TEST_USER PSQL_BIN PGHOST PGHOSTADDR PGPORT PGDATABASE PGUSER PGSERVICE PGSERVICEFILE PGOPTIONS INVOICE_TEST_DSN INVOICE_TEST_GUARD_FILE; do
  if [[ -n "${!option+x}" ]]; then
    echo "refusing external connection variable: $option; this script creates its own cluster" >&2
    exit 2
  fi
done
go_integration=false
case "${1:-}" in
  '') red_only=false ;;
  --red-only) red_only=true ;;
  --integration|--admin-flow|--device-flow) red_only=false; go_integration=true ;;
  *) echo 'usage: scripts/test-db.sh [--red-only|--integration|--admin-flow|--device-flow]; PG_TEST_BIN_DIR selects PostgreSQL binaries' >&2; exit 2 ;;
esac
[[ $# -le 1 ]] || { echo 'too many arguments' >&2; exit 2; }
root="$(cd "$(dirname "$0")/.." && pwd)"
if [[ -n "${PG_TEST_BIN_DIR:-}" ]]; then
  [[ "$PG_TEST_BIN_DIR" = /* ]] || { echo 'PG_TEST_BIN_DIR must be absolute' >&2; exit 2; }
  pg_bin_dir="$PG_TEST_BIN_DIR"
else
  initdb_path="$(command -v initdb)" || { echo 'initdb missing; set PG_TEST_BIN_DIR to an installed PostgreSQL bin directory' >&2; exit 2; }
  pg_bin_dir="$(dirname "$initdb_path")"
fi
for tool in initdb pg_ctl postgres psql createdb; do
  [[ -x "$pg_bin_dir/$tool" ]] || { echo "missing PostgreSQL binary: $pg_bin_dir/$tool" >&2; exit 2; }
done
# A script-created database in a script-created cluster is the isolation proof.
# No external target, prefix heuristic, schema emptiness guess or reuse mode exists.
runtime="$root/.runtime"
[[ ! -L "$runtime" ]] || { echo 'refusing symlinked .runtime' >&2; exit 2; }
mkdir -p "$runtime"
[[ -O "$runtime" && -d "$runtime" ]] || { echo '.runtime must be owned by the current user' >&2; exit 2; }
run_dir="$(mktemp -d "$runtime/db-test.XXXXXX")"
socket_dir=''
data_dir="$run_dir/data"
cleanup() {
  result=$?
  trap - EXIT INT TERM
  # These paths were minted by this invocation, never taken from a connection env.
  if [[ -d "$data_dir" && ! -L "$data_dir" && -f "$data_dir/postmaster.pid" ]]; then
    if ! "$pg_bin_dir/pg_ctl" -D "$data_dir" -m immediate -w stop >>"$run_dir/cleanup.log" 2>&1; then
      echo "could not stop this invocation's test cluster; retained $data_dir for recovery" >&2
      [[ $result -ne 0 ]] || result=1
      echo "test logs: $run_dir" >&2
      exit "$result"
    fi
  fi
  [[ -z "$socket_dir" ]] || rm -rf -- "$socket_dir"
  rm -rf -- "$data_dir"
  echo "test logs: $run_dir" >&2
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
socket_dir="$(mktemp -d /tmp/invoice-db.XXXXXX)"
port=65432
owner=invoice_test_owner
database="invoice_test_${run_dir##*.}"
printf '%s\n' "pid=$$" "data=$data_dir" "socket=$socket_dir" "database=$database" >"$run_dir/owned-resources.txt"
if ! "$pg_bin_dir/initdb" -D "$data_dir" --username="$owner" --encoding=UTF8 --no-locale --auth-local=trust --auth-host=reject --no-instructions >"$run_dir/initdb.log" 2>&1; then
  cat "$run_dir/initdb.log" >&2
  echo 'BLOCKED: private cluster initdb failed; no database behavior tests ran' >&2
  exit 1
fi
# TCP is disabled. A short, mode-0700 random Unix socket directory belongs only to this run.
if ! "$pg_bin_dir/pg_ctl" -D "$data_dir" -l "$run_dir/postgres.log" -w start -o "-c listen_addresses='' -c unix_socket_directories='$socket_dir' -c unix_socket_permissions=0700 -p $port" >"$run_dir/start.log" 2>&1; then
  cat "$run_dir/start.log" >&2
  echo 'BLOCKED: private cluster startup failed; no database behavior tests ran' >&2
  exit 1
fi
"$pg_bin_dir/createdb" -h "$socket_dir" -p "$port" -U "$owner" --template=template0 "$database"
args=(-X -v ON_ERROR_STOP=1 -v VERBOSITY=verbose -h "$socket_dir" -p "$port" -U "$owner" -d "$database")
"$pg_bin_dir/psql" "${args[@]}" -c 'SELECT version(), current_database(), inet_server_addr();'
# Exercise the real suite in an empty new DB before any migration. Verify the actual failure.
if "$pg_bin_dir/psql" "${args[@]}" -c 'SET search_path=public,pg_catalog' -f "$root/backend/db/tests/constraints.sql" >"$run_dir/red.log" 2>&1; then
  echo 'FAIL: empty-database suite unexpectedly succeeded' >&2
  exit 1
fi
red_output="$(cat "$run_dir/red.log")"
if [[ "$red_output" != *'42P01: relation "users" does not exist'* ]]; then
  cat "$run_dir/red.log" >&2
  echo 'FAIL: empty-database failure was not the expected undefined users table' >&2
  exit 1
fi
echo 'PASS empty-database RED observed (42P01 users absent)'
[[ "$red_only" = false ]] || exit 0
# Every migration shares one transaction and a fixed creation/lookup schema.
migration_args=(--single-transaction -c 'SET LOCAL search_path=public,pg_catalog')
for migration in "$root"/backend/db/migrations/[0-9][0-9][0-9]_*.sql; do
  migration_args+=(-f "$migration")
done
"$pg_bin_dir/psql" "${args[@]}" "${migration_args[@]}" >"$run_dir/migrations.log" 2>&1 || {
  cat "$run_dir/migrations.log" >&2
  echo 'FAIL: migration transaction rolled back' >&2
  exit 1
}
"$pg_bin_dir/psql" "${args[@]}" -c 'SET search_path=public,pg_catalog' -f "$root/backend/db/tests/constraints.sql"
if [[ "$go_integration" = true ]]; then
  # Proof of this invocation's ownership is checked before Go tests can write.
  # This marker is a harness table, not an application migration or auth secret.
  nonce="$("$pg_bin_dir/psql" "${args[@]}" -At -c 'SELECT gen_random_uuid()')"
  printf '%s\n' "$nonce" "$database" "$data_dir" >"$run_dir/guard"
  "$pg_bin_dir/psql" "${args[@]}" -v guard_nonce="$nonce" <<'SQL'
CREATE TABLE public.invoice_test_guard(nonce uuid PRIMARY KEY);
INSERT INTO public.invoice_test_guard(nonce) VALUES (:'guard_nonce'::uuid);
SQL
  go_bin="${GO_BIN:-go}"
  test_args=(-tags=integration ./internal/... -count=1 -timeout=180s)
  if [[ "${1:-}" = --admin-flow ]]; then
    test_args=(-tags=integration,browser ./internal/httpapi -run '^TestAdminFlow$' -count=1 -timeout=180s -v)
  fi
  if [[ "${1:-}" = --device-flow ]]; then
    test_args=(-tags=integration,browser ./internal/httpapi -run '^TestDeviceFlow$' -count=1 -timeout=180s -v)
  fi
  cd "$root/backend"
  INVOICE_TEST_DSN="host=$socket_dir port=$port user=$owner dbname=$database sslmode=disable connect_timeout=5" \
  INVOICE_TEST_GUARD_FILE="$run_dir/guard" \
  INVOICE_GO_BIN="$go_bin" \
  "$go_bin" test "${test_args[@]}"
fi
