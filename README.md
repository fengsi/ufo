# UFO: Unified Fleet Orchestrator

> An open-source zero-human ops platform 🦾🩶

[![CI](https://img.shields.io/github/actions/workflow/status/fengsi/ufo/ci.yml?branch=main&style=flat-square&label=CI)](https://github.com/fengsi/ufo/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/fengsi/ufo?style=flat-square)](https://github.com/fengsi/ufo/releases)
[![crates.io](https://img.shields.io/crates/v/ufo-cli?style=flat-square)](https://crates.io/crates/ufo-cli)
[![License](https://img.shields.io/github/license/fengsi/ufo?style=flat-square)](LICENSE)
[![Preview](https://img.shields.io/badge/status-preview-blue?style=flat-square)](CHANGELOG.md)
[![Go](https://img.shields.io/badge/Go-1.26%2B-00ADD8?style=flat-square)](apps/api/go.mod)
[![Node](https://img.shields.io/badge/Node-20.9%2B-5FA04E?style=flat-square)](apps/web/package.json)
[![Rust](https://img.shields.io/badge/Rust-2024-B7410E?style=flat-square)](apps/rover/Cargo.toml)
[![Gitmoji](https://img.shields.io/badge/commits-gitmoji-FDD563?style=flat-square)](https://gitmoji.dev)

UFO is an operations board that keeps execution on enrolled rovers. The control
plane tracks fleets, missions, conversations, runs, and review handoffs; rovers
are the host-side runtimes that do the work. Assign an operation to a pilot
(Claude or Codex today), and a rover invokes the local CLI in an isolated work
dir, streams progress, and returns a final message plus git diff for review.

> [!WARNING]
> **MVP preview:** UFO's main workflow is functional, but compatibility is not
> guaranteed yet. APIs, the database schema, configuration, and rover protocol
> may change without notice. Upgrading may require resetting the database; a
> migration path between commits or releases is not guaranteed. Do not use this
> preview for data you cannot afford to lose.

See [`CHANGELOG.md`](CHANGELOG.md) for release history.

---

## Architecture

UFO is multi-tenant: users sign in, and **fleets** scope all data. **Missions**
group related operations and provide short keys like `MSJ`, producing operation
codes such as `MSJ-123`.

```
Control plane

┌─────────────┐    ┌─────────────┐    ┌─────────────┐       ┌─────────────┐
│ Browser     │◀──▶│ Next.js web │◀──▶│ Go API      │◀─SQL─▶│ PostgreSQL  │
│ operations  │    │ /api proxy  │    │ control     │       │             │
│ board       │    │             │    │ plane       │       │             │
└─────────────┘    └─────────────┘    └──────┬──────┘       └─────────────┘
                                             │ fleet-scoped HTTP
                                             │ claims / progress
                                             │ results / artifacts
Execution plane (rover host) ────────────────┼─────────────────────────────
                                             ↕
                                      ┌─────────────┐
                                      │ Rust rover  │
                                      └──────┬──────┘
                                             │ invokes
                                             ▼
                                      ┌─────────────┐
                                      │ pilot CLI   │
                                      └──────┬──────┘
                                             │ works in
                                             ▼
                               ┌───────────────────────────┐
                               │ per-operation work dir    │
                               └───────────────────────────┘
```

- **`apps/web`** — Next.js product UI: a default drag-and-drop **Kanban** board
  plus **List** and **Lanes** views; operation detail pages with conversations,
  live run timelines, diffs, labels, reactions, sub-operations, relationships,
  and **Signals**. Proxies `/api`.
- **`apps/api`** — Go control plane (pgx + sqlc): auth, fleets, memberships,
  invitations, pilots, crews, operations, comments, runs, artifacts, missions,
  labels, reactions, signals, rover enrollment, and connection-token endpoints.
- **`apps/rover`** — Rust CLI rover: enrolls via an enrollment code, long-poll
  claims runs, invokes the assigned pilot CLI, streams typed messages, uploads a
  `git diff`, and reports terminal state. One host can hold many registrations.
- **`db/`** — SQL migrations and sqlc queries.
- **[`packages/protocol/openapi.yaml`](packages/protocol/openapi.yaml)** —
  OpenAPI source of truth for the API.

### Capabilities

- **Accounts + tenancy:** email/password + cookie sessions; **fleets** +
  memberships scope every entity; invite teammates by email (owner/admin/member).
- **Rovers as teammates:** each rover has its own connection token, reports
  online/busy/offline status, and receives work only when its tags match.
- **Pilots as teammates:** pilots are first-class assignable entities backed by
  local Claude or Codex CLIs. Crews can include pilots and humans; assigning to a
  pilot or pilot-backed crew auto-dispatches, while human-only work stays in
  **backlog**.
- **Operations as conversations + review handoff:** pilots work in resumable
  sessions, stream typed telemetry, return a diff artifact, and hand successful
  runs to **In Review** instead of auto-closing them.
- **Board:** Kanban, List, and Lanes views with configurable columns, filters,
  sorting, labels, reactions, sub-operations, relationships, and signals for
  review handoffs or blocked work.
- **Real-time over PostgreSQL `LISTEN/NOTIFY`:** WebSocket UI updates and rover
  long-polling share the database as the coordination layer; no extra broker is
  required.
- **Orphaned-run lease:** rover heartbeats; an API sweeper requeues silent runs
  (`UFO_RUN_LEASE_SECONDS`, default 30).
- **Multi-instance-safe:** versioned migrator under a `pg_advisory_lock`, claim
  via `FOR UPDATE SKIP LOCKED`, events ordered by insertion id, stateless API.

> **Trust boundary:** anyone in a fleet can dispatch work to connected rovers.
> Pilots run local CLIs with the rover user's privileges. Use dedicated users or
> hosts for rovers, and read [`SECURITY.md`](SECURITY.md) before sharing a
> fleet.

---

## Prerequisites

- **Docker** — runs PostgreSQL, the API, and the web board.
- **Rust / Cargo** — the rover always runs on the host (it's the local runtime).

Only needed for the optional host-based dev path (running api/web without Docker):

- Go ≥ 1.26, Node ≥ 20.9 (Next 16 requires it), and `sqlc` (`brew install sqlc`,
  only if you change SQL).

## Running it

**Recommended — Docker for everything except the rover:**

```bash
scripts/dev.sh up        # docker: PostgreSQL + automatically rebuilt api + web
```

If a preview update changes `0001_init.sql`, reset the local database before
starting again:

```bash
scripts/dev.sh down -v   # deletes the local PostgreSQL volume and all UFO data
scripts/dev.sh up
```

1. Open <http://localhost:3000> and **sign up** — a fleet is created for you.
2. Open the **Rovers** panel → **Add rover** → copy the `UFO_ENROLLMENT_CODE=…` line.
3. Start the rover on the host (it's the local runtime — touches host files/tools —
   and reaches the dockerized API at `localhost:8080`). It enrolls on first run and
   stores each registration (keyed by rover id) in `~/.ufo/rovers.json`; later
   runs use the stored connection token:

   ```bash
   UFO_ENROLLMENT_CODE=<code> scripts/dev.sh rover    # first run (enrolls + starts)
   scripts/dev.sh rover                                  # starts all registered rovers
   ```

   A host can hold many registrations (across fleets/servers); manage them with:

   ```bash
   # from the repo root (the rover crate lives in apps/rover):
   cargo run --manifest-path apps/rover/Cargo.toml -- rover list                 # show registrations
   cargo run --manifest-path apps/rover/Cargo.toml -- rover remove <rover-id|prefix> # deregister one
   cargo run --manifest-path apps/rover/Cargo.toml -- rover remove --all         # deregister all
   ```
4. Create a mission, then an operation on the board, assign it to a pilot, and
   watch the run move `queued → claimed → running → succeeded` live, with its diff
   artifact. The rover shows **online/busy/offline** in the Rovers panel.

**Alternative — everything on the host** (needs Go + Node ≥ 20.9 installed),
one process per terminal (`api`, `web`, then sign up and run `rover` with the
enrollment code):

```bash
# docker: PostgreSQL only
scripts/dev.sh db

# host: Go API
scripts/dev.sh api

# host: Next.js dev server
scripts/dev.sh web

# host: Rust rover (enrolls)
UFO_ENROLLMENT_CODE=<code> scripts/dev.sh rover
```

### Configuration

Copy `.env.example` to `.env` to override defaults:

| Var | Default | Used by |
| --- | --- | --- |
| `DATABASE_URL` | `postgres://ufo:ufo@localhost:5432/ufo?sslmode=disable` | api |
| `UFO_API_ADDR` | `:8080` | api |
| `UFO_RUN_LEASE_SECONDS` | `30` | api |
| `UFO_LONGPOLL_SECONDS` | `25` | api |
| `UFO_SECURE_COOKIES` | _(unset)_ — set when serving over HTTPS | api |
| `UFO_WEB_ORIGIN` | _(unset)_ — CORS + WebSocket origin allowlist; set in production | api |
| `API_PROXY_TARGET` | `http://localhost:8080` (build arg `http://api:8080` in Docker) | web |
| `UFO_SERVER` | `http://localhost:8080` | rover |
| `UFO_ENROLLMENT_CODE` | _(from the Rovers panel; used by `rover enroll`)_ | rover |
| `UFO_OUTPOST` | `~/.ufo` (op trees: `<outpost>/rovers/<rover-id>/operations/<operation-id>`) | rover |

### Regenerating DB code

After editing `db/migrations/*.sql` or `db/queries/*.sql`:

```bash
sqlc generate    # regenerates apps/api/internal/db
```

## Quick API smoke test (curl)

The UI surface needs a session cookie and a `?fleet=`. Public ids are strings,
so keep them quoted in JSON bodies.

```bash
# sign up (saves the session cookie); a fleet is created for you
curl -s -c jar -X POST localhost:8080/api/auth/signup \
  -H 'Content-Type: application/json' \
  -d '{"email":"me@example.com","password":"P@ssw0rd","name":"Me"}'

FLEET=$(curl -s -b jar localhost:8080/api/fleets | python3 -c 'import sys,json;print(json.load(sys.stdin)[0]["id"])')

# a mission groups operations (required to create one)
MISSION=$(curl -s -b jar -X POST "localhost:8080/api/missions?fleet=$FLEET" \
  -H 'Content-Type: application/json' -d '{"name":"Moonshot","key":"MSJ"}' \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')

# create a pilot (Claude or Codex), then assign an operation to it → it auto-runs
# once a rover advertising the matching pilot CLI is online
PILOT=$(curl -s -b jar -X POST "localhost:8080/api/pilots?fleet=$FLEET" \
  -H 'Content-Type: application/json' -d '{"name":"cc","kind":"claude"}' \
  | python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
curl -s -b jar -X POST "localhost:8080/api/operations?fleet=$FLEET" \
  -H 'Content-Type: application/json' \
  -d "{\"title\":\"hello\",\"body\":\"Summarize this repo\",\"mission_id\":\"$MISSION\",\"assignee_type\":\"pilot\",\"assignee_id\":\"$PILOT\"}"
curl -s -b jar "localhost:8080/api/runs?fleet=$FLEET"            # runs in this fleet
```

## License

BSD 3-Clause. See [LICENSE](LICENSE) and
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
