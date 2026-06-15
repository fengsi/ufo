#!/usr/bin/env bash
# Bring up the UFO walking-skeleton dev stack.
#
# Usage:
#   scripts/dev.sh db        # start PostgreSQL (docker compose) and wait for health
#   scripts/dev.sh api       # run the Go API server (needs db up)
#   scripts/dev.sh rover    # run the Rust rover (needs api up)
#   scripts/dev.sh web       # run the Next.js web board (needs api up)
#
# Run each in its own terminal. Env defaults come from .env if present.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# Load .env if present (export all keys).
if [[ -f .env ]]; then
  set -a; # shellcheck disable=SC1091
  source .env; set +a
fi

export PATH="/opt/homebrew/bin:$PATH"

: "${DATABASE_URL:=postgres://ufo:ufo@localhost:5432/ufo?sslmode=disable}"
: "${UFO_API_ADDR:=:8080}"
: "${UFO_SERVER:=http://localhost:8080}"
: "${API_PROXY_TARGET:=http://localhost:8080}"
export DATABASE_URL UFO_API_ADDR UFO_SERVER API_PROXY_TARGET

cmd="${1:-}"
case "$cmd" in
  db)
    docker compose up -d --wait postgres   # --wait blocks until healthy
    echo "PostgreSQL is healthy at $DATABASE_URL"
    ;;
  api)
    cd apps/api
    go run ./cmd/api
    ;;
  rover)
    # First run: UFO_ENROLLMENT_CODE=<code> scripts/dev.sh rover  (enrolls a new
    # rover under its id, then starts). After that, `scripts/dev.sh rover` runs all
    # registered rovers (~/.ufo/rovers.json) concurrently.
    cd apps/rover
    if [[ -n "${UFO_ENROLLMENT_CODE:-}" ]]; then
      cargo run -- rover enroll --server "${UFO_SERVER}" --enrollment-code "${UFO_ENROLLMENT_CODE}"
    fi
    cargo run -- rover start
    ;;
  web)
    cd apps/web
    [[ -d node_modules ]] || npm install
    npm run dev
    ;;
  up)
    # Everything except the rover, rebuilding changed images automatically.
    docker compose up --build --watch "${@:2}"
    ;;
  down)
    docker compose down "${@:2}"
    ;;
  *)
    echo "usage: scripts/dev.sh {up|down|db|api|rover|web}" >&2
    echo "  up        docker: PostgreSQL + automatically rebuilt API + web" >&2
    echo "  down      stop the docker stack" >&2
    echo "  db        docker: PostgreSQL only" >&2
    echo "  api       host: Go API (go run)" >&2
    echo "  rover    host: Rust rover (cargo run)" >&2
    echo "  web       host: Next.js dev server" >&2
    exit 2
    ;;
esac
