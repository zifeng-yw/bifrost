#!/usr/bin/env bash

set -euo pipefail

umask 077

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
env_file="$script_dir/.env"
database_file="$script_dir/data/config.db"
rotate=false
build=false
show=false
generated=false
tmp_env=""
cookie_jar=""

usage() {
  cat <<'EOF'
Usage: ./start.sh [--rotate] [--build] [--show]

  --rotate  Generate a new admin password.
  --build   Rebuild the local Docker image before starting.
  --show    Print the active credentials after startup.
EOF
}

cleanup() {
  if [[ -n "$tmp_env" ]]; then
    rm -f "$tmp_env"
  fi
  if [[ -n "$cookie_jar" ]]; then
    rm -f "$cookie_jar"
  fi
}

trap cleanup EXIT

read_env_value() {
  local key=$1
  awk -v key="$key" '
    index($0, key "=") == 1 {
      print substr($0, length(key) + 2)
      exit
    }
  ' "$env_file"
}

require_value() {
  local name=$1
  local value=$2
  if [[ -z "$value" ]]; then
    echo "Error: $name is missing from $env_file" >&2
    exit 1
  fi
}

for arg in "$@"; do
  case "$arg" in
    --rotate) rotate=true ;;
    --build) build=true ;;
    --show) show=true ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      echo "Unknown option: $arg" >&2
      usage >&2
      exit 1
      ;;
  esac
done

for command in docker openssl curl awk; do
  if ! command -v "$command" >/dev/null 2>&1; then
    echo "Error: required command not found: $command" >&2
    exit 1
  fi
done

if [[ ! -f "$env_file" && -f "$database_file" ]]; then
  echo "Error: $database_file exists but $env_file is missing." >&2
  echo "Restore the original .env so the persisted encryption key is not lost." >&2
  exit 1
fi

if [[ -f "$env_file" ]]; then
  admin_username=$(read_env_value BIFROST_ADMIN_USERNAME)
  admin_password=$(read_env_value BIFROST_ADMIN_PASSWORD)
  encryption_key=$(read_env_value BIFROST_ENCRYPTION_KEY)
  bind_address=$(read_env_value BIFROST_BIND_ADDRESS)
else
  admin_username="admin"
  admin_password=""
  encryption_key=$(openssl rand -hex 32)
  bind_address=127.0.0.1
fi

if [[ ! -f "$env_file" || "$rotate" == true ]]; then
  admin_password="Bf!$(openssl rand -hex 24)"
  tmp_env=$(mktemp "$script_dir/.env.tmp.XXXXXX")
  {
    printf 'BIFROST_ADMIN_USERNAME=%s\n' "$admin_username"
    printf 'BIFROST_ADMIN_PASSWORD=%s\n' "$admin_password"
    printf 'BIFROST_ENCRYPTION_KEY=%s\n' "$encryption_key"
    printf 'BIFROST_BIND_ADDRESS=%s\n' "${bind_address:-127.0.0.1}"
  } >"$tmp_env"
  chmod 600 "$tmp_env"
  mv "$tmp_env" "$env_file"
  tmp_env=""
  generated=true
fi

require_value BIFROST_ADMIN_USERNAME "$admin_username"
require_value BIFROST_ADMIN_PASSWORD "$admin_password"
require_value BIFROST_ENCRYPTION_KEY "$encryption_key"
chmod 600 "$env_file"

compose_args=(up -d)
if [[ "$build" == true ]]; then
  compose_args+=(--build)
fi
if [[ "$generated" == true ]]; then
  compose_args+=(--force-recreate)
fi

(
  cd "$script_dir"
  docker compose config --quiet
  docker compose "${compose_args[@]}"
)

for _ in $(seq 1 60); do
  health_status=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' bifrost 2>/dev/null || true)
  if [[ "$health_status" == healthy ]]; then
    break
  fi
  if [[ "$health_status" == unhealthy ]]; then
    echo "Error: Bifrost became unhealthy." >&2
    (cd "$script_dir" && docker compose logs --tail 100 bifrost) >&2
    exit 1
  fi
  sleep 1
done

if [[ "${health_status:-}" != healthy ]]; then
  echo "Error: timed out waiting for Bifrost to become healthy." >&2
  exit 1
fi

cookie_jar=$(mktemp "${TMPDIR:-/tmp}/bifrost-cookie.XXXXXX")
login_payload=$(printf '{"username":"%s","password":"%s"}' "$admin_username" "$admin_password")
login_code=$(curl -sS -o /dev/null -c "$cookie_jar" -w '%{http_code}' \
  -H 'Content-Type: application/json' \
  --data "$login_payload" \
  http://127.0.0.1:8080/api/session/login)
if [[ "$login_code" != 200 ]]; then
  echo "Error: dashboard credential verification returned HTTP $login_code." >&2
  exit 1
fi
curl -sS -o /dev/null -b "$cookie_jar" -X POST http://127.0.0.1:8080/api/session/logout

echo "Bifrost is healthy at http://127.0.0.1:8080"
echo "Credentials are stored in $env_file with mode 600."

if [[ "$show" == true ]]; then
  printf '\nAdmin username: %s\n' "$admin_username"
  printf 'Admin password: %s\n' "$admin_password"
fi
