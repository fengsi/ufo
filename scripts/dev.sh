#!/bin/sh
# Bring up the UFO dev stack.
#
# Usage:
#   scripts/dev.sh up        # Docker (live watch): PostgreSQL + API + web
#   scripts/dev.sh down      # stop the Docker Compose stack
#   scripts/dev.sh db        # PostgreSQL only (Docker), wait for health
#   scripts/dev.sh api       # host: Go Hub (needs db up)
#   scripts/dev.sh web       # host: Next.js web board (needs api up)
#   scripts/dev.sh rover           # host: run enrolled rovers (rover start)
#   scripts/dev.sh rover enroll    # host: web-enroll (approve in browser), then start
#   scripts/dev.sh rover enroll --headless  # enroll, then start headless
#
# `up` is the default. The host commands are the all-local fallback (Go + Node).
# Env defaults come from .env if present.
set -eu

ROOT="$(CDPATH= cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

rover_enrollment_code_was_set=0
if [ "${UFO_ROVER_ENROLLMENT_CODE+x}" = "x" ]; then
  rover_enrollment_code_env="$UFO_ROVER_ENROLLMENT_CODE"
  rover_enrollment_code_was_set=1
fi

# Load .env if present (export all keys).
if [ -f .env ]; then
  set -a
  # shellcheck disable=SC1091
  . ./.env
  set +a
fi
if [ "$rover_enrollment_code_was_set" = 1 ]; then
  export UFO_ROVER_ENROLLMENT_CODE="$rover_enrollment_code_env"
fi

: "${UFO_HUB_DATABASE_URL:=postgres://ufo:ufo@localhost:5432/ufo?sslmode=disable}"
: "${UFO_HUB_BIND:=:8080}"
: "${UFO_HUB_URL:=http://localhost:8080}"
: "${UFO_HUB_ALLOWED_ORIGINS:=http://localhost:3000,http://127.0.0.1:3000}"
: "${UFO_HUB_WEB_URL:=http://localhost:3000}"
export UFO_HUB_DATABASE_URL UFO_HUB_BIND UFO_HUB_URL UFO_HUB_ALLOWED_ORIGINS UFO_HUB_WEB_URL

quote_arg() {
  printf "'%s'" "$(printf "%s" "$1" | sed "s/'/'\\\\''/g")"
}

append_quoted_arg() {
  quoted=$(quote_arg "$2")
  eval "$1=\"\${$1} \$quoted\""
}

cmd="${1:-}"
[ "$#" -gt 0 ] && shift
case "$cmd" in
  db)
    docker compose up -d --wait postgres   # --wait blocks until healthy
    echo "PostgreSQL is healthy at $UFO_HUB_DATABASE_URL"
    ;;
  api)
    cd apps/api
    go run ./cmd/api
    ;;
  rover)
    # First run, code: UFO_ROVER_ENROLLMENT_CODE=<code> scripts/dev.sh rover enroll
    # First run, web:  scripts/dev.sh rover enroll [--name NAME --units N --tag TAG]
    #   (opens browser approval when possible, then starts after approval)
    # After enrolling, `scripts/dev.sh rover` runs all enrolled rovers concurrently.
    if [ "${1:-}" = "enroll" ]; then
      shift
      enroll_args=""
      while [ "$#" -gt 0 ]; do
        if [ "$1" = "--" ]; then
          shift
          continue
        fi
        append_quoted_arg enroll_args "$1"
        shift
      done
      enroll_cmd="cargo run --manifest-path apps/rover/Cargo.toml -- rover enroll --hub $(quote_arg "$UFO_HUB_URL")"
      if [ -n "${UFO_ROVER_ENROLLMENT_CODE:-}" ]; then
        enroll_cmd="$enroll_cmd --enrollment-code $(quote_arg "$UFO_ROVER_ENROLLMENT_CODE")"
      fi
      eval "$enroll_cmd$enroll_args"
    else
      cargo run --manifest-path apps/rover/Cargo.toml -- rover start "$@"
    fi
    ;;
  web)
    cd apps/web
    [ -d node_modules ] || npm install
    npm run dev
    ;;
  up|"")
    # Builds dev images, starts the stack, and live-syncs source on change
    # (next dev Fast Refresh; go run restarts on edit).
    docker compose up --build --watch "$@"
    ;;
  down)
    docker compose down "$@"
    ;;
  *)
    echo "usage: scripts/dev.sh {up|down|db|api|web|rover}" >&2
    echo "  up        Docker (live watch): PostgreSQL + API + web   [default]" >&2
    echo "  down      stop the Docker Compose stack" >&2
    echo "  db        Docker: PostgreSQL only" >&2
    echo "  api       host: Go Hub (go run)" >&2
    echo "  web       host: Next.js dev server" >&2
    echo "  rover     host: run enrolled rovers; 'rover enroll' web-enrolls then starts" >&2
    echo "            e.g. 'rover enroll --headless'" >&2
    exit 2
    ;;
esac
