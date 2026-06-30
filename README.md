# UFO: Unified Fleet Orchestrator

**An open-source zero-human ops platform** 🦾🩶

<p align="center">
  <img src=".github/assets/readme-banner.jpg" alt="UFO coordinating a fleet of local rovers from orbit" width="100%">
</p>

[![Build](https://img.shields.io/github/actions/workflow/status/fengsi/ufo/ci.yml?logo=github&style=for-the-badge)](https://github.com/fengsi/ufo/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/fengsi/ufo?style=for-the-badge)](https://github.com/fengsi/ufo/releases)
[![crates.io](https://img.shields.io/crates/v/ufo-cli?style=for-the-badge)](https://crates.io/crates/ufo-cli)
[![License](https://img.shields.io/github/license/fengsi/ufo?style=for-the-badge)](LICENSE)
[![Status](https://img.shields.io/badge/status-beta-blue?style=for-the-badge)](CHANGELOG.md)
[![Go](https://img.shields.io/badge/Go-1.26%2B-00ADD8?logo=go&style=for-the-badge)](apps/api/go.mod)
[![Node](https://img.shields.io/badge/Node-20.9%2B-5FA04E?logo=node.js&style=for-the-badge)](apps/web/package.json)
[![Rust](https://img.shields.io/badge/Rust-2024-B7410E?logo=rust&style=for-the-badge)](apps/rover/Cargo.toml)
[![Gitmoji](https://img.shields.io/badge/commits-gitmoji-FDD563?style=for-the-badge)](https://gitmoji.dev)

Most AI workflows treat work as an ephemeral thread: a tab closes, a process
ends, and the memory vanishes. The industry's reflex has been to build better
plumbing by standardizing the pipes between models and tools, or
daisy-chaining local assistants to pass context down the wire. But passing
state through a pipe doesn't cure a stateless architecture; it just scales the
amnesia. When the chain snaps, the shared context falls apart.
Hyper-optimizing the solo operator has hit its scaling limit. An asynchronous
workforce bound to synchronous manual coordination will always block at the
speed of the human in the loop.

UFO (Unified Fleet Orchestrator) shifts the center of gravity. Instead of
shuttling work between transient agents, it anchors agents to the work. On the
surface, it feels like a calm operations board; underneath, it is shared
institutional memory. Whether you are automating a routine chore or
architecting a sprawling greenfield platform, the pattern is the same. A rover
picks up the task and hands the controls to an AI agent acting as your pilot.
It handles the heavy lifting in an isolated workspace and brings the finished
work back to the board, whether that's a simple answer or an end-to-end
solution. Routines ground the system, turning ad-hoc prompts into compounding
momentum.

> [!NOTE]
> **Public beta:** UFO's core workflow is usable. Release notes call out
> upgrade caveats for each tagged release; before 1.0, APIs, the database
> schema, configuration, storage paths, and the rover protocol may still
> change. Back up before upgrading, especially when testing arbitrary source
> commits against data you care about.

See [`CHANGELOG.md`](CHANGELOG.md) for release history.

## Architecture

UFO is built around the fleet. Users bring intent as operations, missions give
that work a durable frame, the Hub keeps state and policy, and rovers extend
the fleet onto local machines where private context and tools already live.
The goal is zero-human ops over time: routine coordination, execution,
handoff, and follow-up should move from humans nudging work forward to UFO
carrying more of the loop.

Because the unit is an operation rather than a repository or a chat session,
the same model works for general assistant work, team workflows, and software
changes. Conversations, assets, relationships, signals, history, and routines
stay attached to the operation. Routines are still actively evolving, but they
are the foundation for turning schedules, triggers, and shared context into
repeatable operation loops.

When source code is involved, UFO adds a software handoff path: rovers create
isolated git worktrees, stream telemetry, return diffs for review, and can
hand off results by applying changes to source, creating a source branch, or
refreshing from upstream source changes.

Users sign in, and **fleets** scope all data. **Missions** group related
operations and provide short keys like `MSJ`, producing operation codes such
as `MSJ-123`.

```mermaid
flowchart LR
  Browser["Browser<br/>operations board"]

  subgraph Hub["UFO Hub"]
    Web["Next.js web<br/>apps/web<br/>same-origin UI + /api/v1 facade"]
    API["Go API<br/>apps/api<br/>REST, WebSocket, rover protocol"]
  end

  subgraph Data["State + storage"]
    DB[("PostgreSQL<br/>tenancy, queues, leases<br/>LISTEN/NOTIFY")]
    Assets[("Asset storage<br/>local filesystem, S3-compatible, GCS")]
  end

  subgraph Host["Rover host"]
    Source["Source checkout<br/>user repo"]
    Rover["Rust rover<br/>apps/rover<br/>enrolls, claims, heartbeats"]
    Worktree["Operation worktree<br/>isolated loop workspace"]
    Pilot["Local AI CLI pilot<br/>Codex, Claude, Cursor, Copilot, ..."]
  end

  Browser <-->|UI, REST, WebSocket| Web
  Web <-->|/api/v1 rewrite| API
  API <-->|state, dispatch, notifications| DB
  API <-->|upload/download URLs| Assets
  API <-->|enroll, claim runs, stream config<br/>heartbeats, telemetry, results| Rover
  API <-->|claim source handoff actions| Rover

  Rover -. opens approval .-> Browser
  Source -->|git worktree base| Worktree
  Rover -->|prepare + supervise| Worktree
  Rover -->|run assigned pilot| Pilot
  Pilot <-->|tools + file edits| Worktree
  Worktree -->|diff, artifacts, review handoff| Rover
  Worktree <-->|apply to source<br/>create source branch<br/>refresh from source| Source

  classDef user fill:#eef7ff,stroke:#2f6f9f,color:#0f2f44
  classDef hub fill:#f5f0ff,stroke:#7c5cc4,color:#25164a
  classDef data fill:#f0f8f3,stroke:#4f8a62,color:#16391f
  classDef host fill:#fff6e6,stroke:#b7791f,color:#402600
  class Browser user
  class Web,API hub
  class DB,Assets data
  class Source,Rover,Worktree,Pilot host
```

- **`apps/web`** — Next.js product UI: a default drag-and-drop **Kanban**
  board plus **List** and **Lanes** views; operation detail pages with
  conversations, live run timelines, diffs, labels, reactions, sub-operations,
  relationships, and **Signals**. Proxies `/api/v1`.
- **`apps/api`** — Go Hub (pgx + sqlc): auth, fleets, memberships,
  invitations, pilots, crews, operations, comments, runs, artifacts, assets,
  missions, labels, reactions, signals, rover enrollment, and connection-token
  endpoints.
- **`apps/rover`** — Rust CLI (`ufo-cli`): provides the rover command that
  enrolls by browser approval or code, long-poll claims runs, lets the
  assigned pilot drive the rover, streams typed messages, uploads a `git
  diff`, and reports terminal state. One rover can hold many enrollments.
- **`apps/api/internal/migrate/migrations`, `apps/api/internal/db/queries`** —
  SQL migrations (embedded) and sqlc queries.
- **[`apps/api/internal/spec/openapi.yaml`](apps/api/internal/spec/openapi.yaml)**
  — OpenAPI source of truth; embedded and served at `/openapi.yaml`.

### Capabilities

- **Accounts + tenancy:** email/password + httpOnly access JWT cookie;
  **fleets** + memberships scope every entity; invite teammates by email
  (owner/admin/member).
- **Unified operations context:** missions, comments, assets, relationships,
  signals, routines, and history keep team/org context attached to the work
  instead of scattering it across local agent sessions.
- **Routines as loop foundation:** routines are the actively evolving layer
  for turning schedules, triggers, and shared context into recurring operation
  loops.
- **Rovers as teammates:** each rover has its own connection token, reports
  available/full/offline status, and receives work only when its tags match.
- **Pilots drive rovers:** a pilot is a local AI CLI that drives a rover;
  assign an operation to a pilot and a capable fleet rover claims it. Crews
  can include pilots and humans; assigning to a pilot or pilot-backed crew
  auto-dispatches, while human-only work stays in **backlog**. If the pilot
  has no rover to drive in the fleet, the operation is blocked with a signal
  instead of queueing forever.
- **Operations as conversations + review handoff:** pilots work in resumable
  sessions, stream typed telemetry, return a diff artifact, and hand
  successful runs to **In Review** instead of auto-closing them. Operation
  bodies, comments, and routines can reference uploaded assets backed by local
  storage in dev or object storage in production.
- **Source/worktree handoff:** rover-hosted source checkouts stay local; UFO
  creates per-operation git worktrees, then lets reviewers refresh from
  source, apply worktree changes back to source, or commit them to a source
  branch.
- **Planning dates vs lifecycle time:** operation `start_date` / `due_date`
  are editable planning dates; `started_at` / `finished_at` are UTC lifecycle
  timestamps set by status changes.
- **Board:** Kanban, List, and Lanes views with configurable columns, filters,
  sorting, labels, reactions, sub-operations, relationships, and signals for
  review handoffs or blocked work.
- **Real-time over PostgreSQL `LISTEN/NOTIFY`:** WebSocket UI updates and
  rover long-polling share the database as the coordination layer; no extra
  broker is required.
- **Orphaned-run lease:** rover heartbeats; an API sweeper requeues silent
  runs (`UFO_HUB_RUN_LEASE_SECONDS`, default 30).
- **Multi-instance-safe:** versioned migrator under a `pg_advisory_lock`,
  claim via `FOR UPDATE SKIP LOCKED`, events ordered by insertion id,
  stateless API.

> **Trust boundary:** anyone in a fleet can dispatch work to connected rovers.
> Pilots run local CLIs with the rover user's privileges. Use dedicated users
> or hosts for rovers, and read [`SECURITY.md`](SECURITY.md) before sharing a
> fleet.

---

## Prerequisites

- **Docker** — runs PostgreSQL, the API, and the web board.
- **Rust/Cargo** — runs the rover from this checkout through `scripts/dev.sh
  rover`. The rover always runs on the host because it touches local files and
  tools.

For using the rover outside this source checkout, install the released CLI
instead:

```bash
curl -fsSL https://getufo.dev/install.sh | sh
```

Homebrew also works:

```bash
brew install fengsi/ufo/ufo-cli
```

The rover CI runs on macOS, FreeBSD, Linux, and Windows. Other operating
systems may work, but are not tested yet.

Only needed for the optional host-based dev path (running api/web without
Docker):

- Go ≥ 1.26, Node ≥ 20.9 (Next 16 requires it), and `sqlc` (`brew install
  sqlc`, only if you change SQL).

## Running it

Use the rover command that matches how you are running UFO:

| Context | First run | Later runs |
| --- | --- | --- |
| From this source checkout | `UFO_HUB_URL=<hub-url> scripts/dev.sh rover enroll` | `scripts/dev.sh rover` |
| Installed CLI | `ufo rover enroll --hub <hub-url>` | `ufo rover start` |

The dev wrapper runs the local Rust crate with `cargo run`, points at the
local Hub, and keeps rover work rooted at this checkout. In both forms,
`enroll` starts the rover after approval. See [Rover CLI](apps/rover/README.md)
for installed CLI usage.

**Recommended — Docker for everything except the rover:**

```bash
# Docker (live watch): PostgreSQL + API + web
scripts/dev.sh up
```

Source edits sync into the containers live — the web has Fast Refresh and the
API restarts on change (`docker compose watch`); no manual rebuild. Docker dev
stores local assets in `~/.ufo/assets`, the same default path used when
running the Hub directly, so worktrees share local assets.

Release notes call out upgrade caveats for each tagged release. If you are
testing arbitrary source commits that rewrite the beta schema, back up first;
only discard the Docker volume when you intentionally want to delete local
data.

1. Open <http://localhost:3000> and **sign up** — a fleet is created for you.
2. Start rover enrollment on the host. The dev wrapper opens the Rovers
   page, tells you to sign in if needed, pick the fleet/name/units/tags in the
   approval modal, then waits until approval stores the rover token:

   ```bash
   scripts/dev.sh rover enroll
   ```

   That command enrolls through the browser, then starts the rover in the
   same terminal. For later runs, use `scripts/dev.sh rover`.

   Start the rover from the git checkout you want it to work on. When it sees
   a checkout, each claimed operation runs in a detached git worktree under
   the outpost, with current non-ignored local changes copied in, instead of
   editing the running checkout directly.

   Enrollment stores a connection token (keyed by rover id) in
   `~/.ufo/rovers.json`; later runs use the stored token. For code-based or
   non-browser enrollment, the **Rovers** panel can still create an enrollment
   code for `UFO_ROVER_ENROLLMENT_CODE=<code> scripts/dev.sh rover enroll`.

   In an interactive terminal, the rover start command opens the live rover
   TUI. Use `scripts/dev.sh rover --headless` for later runs, or
   `scripts/dev.sh rover enroll --headless` on first run, for CI,
   launchd/systemd, or old log-oriented output.

   For list/status/remove commands and installed CLI usage, see
   [Rover CLI](apps/rover/README.md).
3. Create a mission, then an operation on the board, assign it to a pilot, and
   watch the run move `queued → claimed → running → succeeded` live, with its
   diff artifact. The rover shows **available/full/offline** in the Rovers
   panel.

**Alternative — everything on the host** (needs Go + Node ≥ 20.9 installed),
one process per terminal (`api`, `web`, then sign up and enroll `rover`):

```bash
# Docker: PostgreSQL only
scripts/dev.sh db

# host: Go API
scripts/dev.sh api

# host: Next.js dev server
scripts/dev.sh web

# host: Rust rover (approve in browser, then starts)
scripts/dev.sh rover enroll
```

### Configuration

Copy `.env.example` to `.env` to override defaults:

| Var | Default | Used by |
| --- | --- | --- |
| `UFO_HUB_URL` | `http://localhost:8080` | rover, web (Hub origin; clients append `/v1`) |
| `UFO_HUB_BIND` | `:8080` | api |
| `UFO_HUB_WEB_URL` | _(unset)_ — web UI base URL advertised by the Hub for browser approval flows | api |
| `UFO_HUB_DATABASE_URL` | `postgres://ufo:ufo@localhost:5432/ufo?sslmode=disable` | api |
| `UFO_HUB_ALLOWED_ORIGINS` | _(unset)_ — CORS + WebSocket origin allowlist; set in production | api |
| `UFO_HUB_SECURE_COOKIES` | _(unset)_ — set when serving over HTTPS | api |
| `UFO_HUB_JWT_PRIVATE_KEY` | _(unset)_ — base64 Ed25519 seed/private key for EdDSA access tokens; unset uses a per-process dev key | api |
| `UFO_HUB_JWT_ACCESS_SECONDS` | `3600` — user access JWT lifetime | api |
| `UFO_HUB_JWT_ISSUER` | `ufo-hub` — user access JWT issuer | api |
| `UFO_HUB_JWT_AUDIENCE` | `ufo-api` — user access JWT audience | api |
| `UFO_HUB_MIN_ROVER_VERSION` | `0.3.1` — oldest rover CLI version allowed to enroll/connect | api |
| `UFO_HUB_MAX_ROVER_VERSION` | _(unset)_ — optional newest rover CLI version allowed to enroll/connect | api |
| `UFO_HUB_RUN_LEASE_SECONDS` | `30` | api |
| `UFO_HUB_LONGPOLL_SECONDS` | `25` | api |
| `UFO_HUB_ROUTINE_SCHEDULER_SECONDS` | `60` — seconds between due scheduled-routine checks | api |
| `UFO_HUB_ROVER_ONLINE_WINDOW_SECONDS` | `60` — seconds since last contact before a rover is offline | api |
| `UFO_HUB_MAX_SUB_OPERATIONS` | `8` — max sub-operations a captain can propose at once | api |
| `UFO_HUB_ASSET_BACKEND` | `local` — use `s3` for AWS S3, Cloudflare R2, MinIO, or another S3-compatible host; use `gcs` for Google Cloud Storage | api |
| `UFO_HUB_ASSET_LOCAL_ROOT` | `~/.ufo/assets` — override the local backend root | api |
| `UFO_HUB_ASSET_UPLOAD_MAX_BYTES` | `26214400` — max bytes per uploaded file | api |
| `UFO_HUB_ASSET_UPLOAD_ALLOWED_CONTENT_TYPES` | _(unset)_ — comma-separated exact types or wildcards like `application/pdf,image/*` | api |
| `UFO_HUB_ASSET_S3_BUCKET` | _(unset)_ — bucket/container for `s3` backend | api |
| `UFO_HUB_ASSET_S3_ENDPOINT` | _(unset)_ — set for Cloudflare R2, MinIO, or custom S3-compatible hosts | api |
| `UFO_HUB_ASSET_S3_REGION` | `auto` | api |
| `UFO_HUB_ASSET_S3_ACCESS_KEY_ID` | _(unset)_ — access key for S3-compatible storage | api |
| `UFO_HUB_ASSET_S3_SECRET_ACCESS_KEY` | _(unset)_ — secret key for S3-compatible storage | api |
| `UFO_HUB_ASSET_S3_PATH_STYLE` | _(unset)_ — set for path-style providers | api |
| `UFO_HUB_ASSET_S3_PREFIX` | _(unset)_ — optional key prefix before `v1/...` | api |
| `UFO_HUB_ASSET_GCS_BUCKET` | _(unset)_ — bucket for `gcs` backend | api |
| `UFO_HUB_ASSET_GCS_CREDENTIALS_FILE` | _(unset)_ — service-account JSON file for `gcs` backend; falls back to `GOOGLE_APPLICATION_CREDENTIALS` | api |
| `UFO_HUB_ASSET_GCS_CREDENTIALS_JSON` | _(unset)_ — service-account JSON value for `gcs` backend; a private key is currently required for signed URLs | api |
| `UFO_HUB_ASSET_GCS_ENDPOINT` | _(unset)_ — optional Google Cloud Storage API endpoint override | api |
| `UFO_HUB_ASSET_GCS_PREFIX` | _(unset)_ — optional object prefix before `v1/...` | api |
| `UFO_HUB_ASSET_SIGNED_URL_SECONDS` | `900` — signed upload/file URL lifetime | api |
| `UFO_ROVER_VERSION` | `latest` — release used by the shell installer and rover upgrade flow | rover |
| `UFO_ROVER_INSTALL_DIR` | `~/.local/bin` — shell-installer target directory | rover |
| `UFO_ROVER_OUTPOST` | `~/.ufo` (operation work directories: `<outpost>/rovers/<rover-id>/operations/<yyyy>/<mm>/<dd>/<operation-id-shard>/<worktree-name>`) | rover |
| `UFO_ROVER_CONFIG` | `~/.ufo/rovers.json` — local enrollment store | rover |
| `UFO_ROVER_ENROLLMENT_CODE` | _(optional fallback code from the Rovers panel; used by `rover enroll`)_ | rover |
| `UFO_ROVER_UNITS` | _(unset)_ — startup units fallback, 1-100, until hub config is available (`--units`) | rover |
| `UFO_ROVER_RETRY_SECONDS` | `1` — wait after a failed claim before retrying | rover |
| `UFO_ROVER_HEARTBEAT_SECONDS` | `5` — run lease-renewal interval | rover |
| `UFO_ROVER_ASSET_UPLOAD_MAX_BYTES` | `104857600` — max bytes for each rover-uploaded referenced file | rover |
| `UFO_ROVER_ASSET_UPLOAD_MAX_FILES` | `20` — max referenced files a rover uploads for one run | rover |
| `UFO_ROVER_AUTO_UPGRADE` | _(unset)_ — set to `1` to let the rover auto-upgrade when the Hub requires it | rover |

## Troubleshooting

- Web does not load: confirm Docker is running, then check `docker compose ps`
  and `docker compose logs -f web api postgres`.
- API cannot reach PostgreSQL: use `scripts/dev.sh up` or `scripts/dev.sh db`,
  and confirm `UFO_HUB_DATABASE_URL` points at the running database.
- Sign-in works but browser calls fail: set `UFO_HUB_ALLOWED_ORIGINS` to the
  web origin and use `UFO_HUB_SECURE_COOKIES=1` only behind HTTPS.
- Rover does not enroll: confirm the Hub URL points at the API origin, then
  approve enrollment in the browser. Use the command form from the table above.
- Rover is online but does not claim work: confirm the operation is assigned
  to a pilot, the pilot CLI is on the rover `PATH`, and tags match.
- Intentionally discard local dev data: `scripts/dev.sh down -v &&
  scripts/dev.sh up` deletes the Docker database volume. Do this only after
  backing up anything you want to keep.

## Documentation

- [Rover CLI](apps/rover/README.md)
- [Security](SECURITY.md)
- [OpenAPI](apps/api/internal/spec/openapi.yaml)
- [Contributing](CONTRIBUTING.md)

## License

BSD 3-Clause. See [LICENSE](LICENSE) and
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
