# Changelog

All notable changes to UFO are recorded here.

> **Public beta:** before 1.0, contracts may still evolve. Prefer tagged
> releases; release notes call out anything that needs a careful upgrade.

## [0.7.8] - 2026-08-10

Public beta maintenance release. Rover CLI **0.7.8+**.

### Pilots

- Added Kilo Code, MastraCode, and Devin as built-in pilots.
- Aligned unattended launch, session resume, and output handling for Codex,
  Antigravity, Grok Build, Cursor Agent, GitHub Copilot, OpenCode, Hermes, Pi,
  Kilo Code, MastraCode, Devin, Kiro, and Kimi with their CLI documentation.

## [0.7.7] - 2026-08-07

Public beta feature release. Adds operation budgets and scoped usage
summaries. Rover CLI **0.7.7+**.

### Budgets & Usage

- Owners and admins can now set per-operation caps on runs, tokens, and USD
  spend. The Hub enforces them when a rover accepts an operation, alongside
  existing fleet, mission, and rover budgets.
- Usage summaries report fleet totals and breakdowns by pilot, rover,
  operation, and UTC day. Mission and operation views can request scoped
  summaries, and operation rows show their spend caps. Usage periods follow
  UTC calendar days, weeks, and months.

## [0.7.6] - 2026-08-01

Public beta maintenance release. Rover CLI **0.7.6+**.

### Pilots

- Added Qoder and Trae as built-in pilots.

## [0.7.5] - 2026-07-25

Public beta feature release. Separates Pulse starts from Loop continuation and
adds Mission learning. Rover CLI **0.7.5+**.

### Routines

- Manual and scheduled Pulses are now independent from Loop continuation.
- The Routines UI now uses a compact list and focused editor.

### Mission Learning

- Completed operations can add reusable learning and doc or skill paths to
  their Mission and future operation prompts.
- Mission learning and metadata updates are race-safe.
- Stored learning is capped at the newest 50 entries per mission. Owners and
  admins can clear learning or delete individual entries from the Missions UI.

## [0.7.3] - 2026-07-12

Public beta maintenance release. Improves forge ship recovery, forge-action
lease renewal, and Hub/Rover version compatibility. Rover CLI **0.7.3+**.

### Forges
- After ship failure, discover a unique open PR/MR on the auto-commit head
  (branch and commit SHA when known). Recovery resumes ship (sync / CI /
  merge); `shipped` means landed, not merely linked.
- Rovers renew forge-action leases while working, stop before an unrenewed
  lease can be re-accepted, and complete idempotently (including heartbeat
  after finalization).

### Hub & Rover
- Hub enforces a rover version floor; invalid min/max rover version env aborts
  Hub startup. Rover requires Hub version 0.7.3+ from `/healthz` and waits for
  a reachable, compatible Hub before accepting work.
- Hub `/healthz` `version` is product semver (release stamp or
  `0.7.3+dev.<sha>` on source builds).

## [0.7.1] - 2026-07-10

Rover patch release.

### Rover CLI
- Fixed pre-commit checks on Windows when Git Bash is installed but WSL is not
  set up.

## [0.7.0] - 2026-07-09

Public beta feature release: **GitHub and GitLab forges**, optional unattended
ship onto a branch or pull request, and migration checksums. Schema expands
for forges; start fresh or migrate a 0.6.x Hub DB manually. Rover CLI
**0.7.0+**.

### Forges
- Owners and admins can connect GitHub or GitLab (including self-hosted API
  bases), bind forges to missions, and manage them from Integrations.
- Rovers push branches and open, update, and merge pull or merge requests with
  host-side credentials; tokens are not stored in the Hub.
- Operation detail shows richer pull-request chips (number, branches, CI /
  status when available).

### Unattended Loops & Ship
- Routines can auto-commit, re-pulse, and optionally ship (open a PR, wait for
  CI, merge to a ship base such as `orbit`, or integrate without a PR).
- Continuous loops keep iterating on progress; ship is opt-in and aimed at
  stall or one-shot land, not every successful auto-commit.

### Hub
- Migration checksums on Hub startup so applied SQL cannot drift under this
  release.

## [0.6.2] - 2026-07-06

Public beta patch release. Rover CLI **0.6.2+**.

### Pilots & Rovers
- Added CodeBuddy Code as a built-in pilot.

### CI
- Removed cross-platform rover binary builds from routine CI.

## [0.6.1] - 2026-07-05

Public beta feature release. Adds email-first onboarding, reusable skills and
repo context, usage and budget controls, safer unattended routine loops, and
English, Simplified Chinese, and Traditional Chinese UI. Prefer a fresh local
Hub DB when coming from 0.5.x unless you migrate the pre-release schema
manually. Rover CLI **0.6.1+**.

### Accounts & Onboarding
- Email/password signup now validates input earlier, creates a personal fleet,
  and opens the board on a seeded **Launch Bay** mission.
- New group fleets also start with a **Launch Bay** mission.
- Sign-in sessions recover more smoothly after an access token expires.
- Failed signup and login attempts no longer leave partial auth state behind.
- Invite links can prefill the signup email, and the web app gives clearer
  feedback when auth or startup checks fail.

### Skills & Repo Context
- Fleet owners and admins can manage reusable skills and attach them to crews
  or operations.
- Pilots receive the selected skills when they accept work, making repeated
  team practices easier to reuse.
- Pilot prompts can include repo context from `.ufo/context/repo.md` so pilots
  start with the project rules that live beside the code.

### Usage & Budgets
- Run history can now show reported tokens, duration, cost, provider, and
  model for completed work.
- Fleet and mission pages include week and month usage summaries.
- Fleets, missions, and rovers can set caps on runs, tokens, or USD spend that
  the Hub enforces before assigning more work.
- UFO notifies the fleet once per budget period when a cap stops new work.
- Budget controls now live on Hub resources instead of rover process settings.

### Routines & Loops
- Routines can continue into follow-up operations automatically when
  configured for a loop.
- Follow-up operations keep a visible link to the previous operation, so loop
  progress is easier to audit.
- Unattended re-pulse loops can commit successful work to a branch before
  starting the next iteration.
- Loops pause when repeated auto-commits make no changes, preventing silent
  churn.
- Captain and unattended-loop prompts now split, consolidate, and verify loop
  work more consistently between iterations.

### Pilot Handoff & Rover
- If a pilot needs input, UFO can try another available pilot before asking a
  human to review the operation.
- The CLI now gives clearer branch and worktree status after automated
  updates.
- Successful unattended-loop commits can clean up their operation worktrees
  automatically.
- Fixed the outpost TUI home screen so dense rover, unit, operation, and event
  lists stay inside the terminal frame instead of scrolling the banner away.

### Web UI
- Added English, Simplified Chinese, and Traditional Chinese UI catalogs.
- UFO follows the browser language by default, remembers the chosen language,
  and includes a compact language switcher.
- Localized the board, operation detail, settings, fleet, member, rover,
  routine, toast, and auth surfaces.
- Added usage summaries and budget editors for fleet, mission, and rover
  scopes.
- Added skill settings and routine controls for unattended re-pulse loops.

### Docs
- Added a Simplified Chinese README.

## [0.5.0] - 2026-07-01

Public beta feature release. Advances the database schema and API surface
(profiles, mission moves, pulses, and related Hub work). Prefer a fresh local
Hub DB when coming from 0.3.x. Rover CLI **0.5.0+**.

### Profiles & Mutual Fleets
- Split signed-in **Me** updates from public **user profiles**
  (`GET /v1/users/{id}`), with profile links from mentions, the board, crews,
  and signals.
- Added **mutual fleets** on user profiles (fleets the viewer shares with that
  user) without exposing unrelated memberships.

### Missions, Operations & Context
- Allowed moving a **main operation** (and its sub-operations) to another
  mission in the **same fleet**: new per-mission sequences, cleared pilot
  sessions so the next run is not a stale resume, system communications, and
  durable `metadata.mission_move` for the properties rail.
- Added a mission-move confirmation in the operation UI that requires typing
  the current display code (`KEY-seq`).
- Stacked **fleet → mission → operation** context into pilot prompts; on
  conflict, operation wins over mission over fleet. After a mission move,
  context always comes from the operation's **current** mission.
- Tightened run lifecycle fences around **`finalized_at`**, accept/result
  paths, and status updates so cancel, heartbeat, and requeue respect terminal
  runs.

### Pulses & Routines
- Added first-class **pulses** for routine execution history (schema, list/get
  APIs, and change notifications) alongside routine pulse creation.

### Rover Enrollment & Outpost
- Aligned flags for single and multi-rover enrollment: `--hub`, `--code`,
  `--name`, `--units` / `UFO_ROVER_UNITS`, and `--tags` (comma-separated; `:`
  allowed inside a tag). Multi-rover enrollment uses repeatable `--config`
  with pipe-separated `key=value` fields and `\|` escapes.
- Applied enroll env fallbacks for both single and multi-rover paths:
  `UFO_HUB_URL`, `UFO_ROVER_ENROLLMENT_CODE`, and `UFO_ROVER_UNITS` after
  explicit flags/fields.
- Kept `ufo rover start` driven by local `rovers.json` and live hub config for
  identity and concurrency, rather than treating enroll-time overrides as the
  long-term source of truth.
- Improved the outpost TUI: separate **hub** and **fleet** columns, a
  **VERSION** banner metric, hub host shown as `host (version)`, and layout
  fixes so list and detail rows fit the frame without spurious ellipsis.
- Standardized ready-for-operation wording in docs and status copy (away from
  "listening" as the product verb).

### Protocol & Deploy
- Expanded the OpenAPI contract for users, operations, assets, routines,
  pulses, labels, mission stats, and run status patches; kept list filters on
  GET query parameters and fleet identity on bodies or resource paths.
- Added Hub database URL helpers and refreshed configuration examples for
  deploy.

## [0.3.1] - 2026-06-30

Rover patch release.

### Rover CLI
- Fixed browser-approved rover enrollment.
- Made `ufo rover enroll` continue into `ufo rover start` after enrollment.
- Simplified `scripts/dev.sh rover enroll` to delegate startup to the rover
  CLI instead of starting a second rover process itself.
- Switched Homebrew install and upgrade guidance to the fully qualified
  `fengsi/ufo/ufo-cli` formula.

## [0.3.0] - 2026-06-30

Public beta release. This release substantially reshapes the database schema,
API surface, rover protocol, and storage model; back up 0.2.x data before
upgrading, and expect to reset dev databases or migrate them manually.

### Accounts, Tenancy & API
- Replaced browser session cookies with httpOnly `ufo_access` JWT cookies and
  EdDSA signing, with production controls for secure cookies, token lifetime,
  issuer, audience, and signing keys.
- Added `/me` profile updates and user public IDs, while preserving fleet
  membership and invitation authorization around owner/admin/member roles.
- Cleaned up REST conventions across mutating APIs: fleet selection now lives
  in request bodies or resource identity, while GET list filters continue to
  use query parameters.
- Split long-lived WebSocket behavior from fleet-scoped REST routes and
  renamed internal WebSocket code away from ambiguous `ws`/`hub` shorthand.
- Expanded the OpenAPI contract for assets, routines, comments, rover config
  streams, source actions, artifact content, JWT auth, and the cleaned-up
  rover endpoints.
- Reworked sqlc queries to list columns explicitly, use named arguments, avoid
  `SELECT *`, and keep timestamp columns in the project order convention.

### Assets & Attachments
- Added first-class assets for real files/blobs, with local filesystem,
  S3-compatible, and Google Cloud Storage backends behind one asset-store
  abstraction.
- Added configurable upload limits, content-type allowlists, signed URL
  lifetimes, local asset roots, S3-compatible settings, and Google Cloud
  Storage service-account settings.
- Added canonical asset APIs: create upload intents with `POST /v1/assets`,
  upload or fetch bytes through `/v1/assets/{id}/file`, resolve asset IDs, and
  list assets referenced by an operation.
- Store object keys under sharded `v1/fleets/...`, `v1/users/...`, and
  `v1/fleets/.../runs/.../artifacts/...` paths without filenames, keeping
  filenames and metadata in PostgreSQL.
- Track asset checksums as JSON by algorithm, using BLAKE3 when UFO reads the
  bytes and preserving vendor checksums such as SHA-256 when object storage
  exposes them.
- Added operation attachments in the web UI, including file picker, paste and
  drag-drop upload, attachment chips, list/grid/compact grid views, source
  filters for user uploads versus pilot outputs, delete confirmation, and
  persisted attachment view preferences.
- Added previews for images, PDFs, Markdown, JSON, code, and text files, with
  modal previews, keyboard navigation, copy actions, and asset links rendered
  inline from operation and comment Markdown.
- Added asset-aware operation creation, comments, routines, and pilot accept
  prompts without inlining file bytes into prompts.

### Routines & Operations
- Added reusable routines with fleet, mission, body, metadata, operation
  metadata, scheduled pulse timestamps, manual pulses, and scheduled operation
  creation.
- Added the Routines view and real-time routine refreshes through the existing
  PostgreSQL notification path.
- Added operation-level asset IDs so files uploaded before submitting an
  operation remain attached and visible.
- Added source action state to operation detail pages, including source repo
  metadata, worktree settings, worktree paths, and pull-request/source action
  history.
- Added artifact previews for large text artifacts: list/detail responses
  carry metadata and preview content, with full content fetched separately.
- Improved comments with create/update/delete routes and paged comment
  previews for dense operation detail screens.

### Source Worktrees & Rover Execution
- Made source-aware rover execution the default when the rover starts inside a
  git checkout: operations run in detached per-operation worktrees under the
  outpost instead of editing the running checkout.
- Copy current non-ignored local checkout changes into each operation worktree
  so pilots see the same source context the human was using.
- Added source actions so reviewers can ask a rover to apply worktree changes
  back to source, create a source branch, or refresh a worktree from source.
- Date-partitioned and sharded rover operation directories under the outpost,
  replacing flat UUID-only paths.
- Added guards around rover-local file references so pilot-generated files are
  uploaded as assets only when the referenced path stays inside the operation
  directory and passes count, size, and type checks.
- Let rovers fetch operation assets from authorized asset URLs before running
  a pilot, and upload generated files referenced in final pilot output back to
  the Hub as assets.
- Kept text communications, final messages, telemetry, logs, and `git.diff`
  artifacts in PostgreSQL instead of fragmenting them into asset blobs.

### Rover CLI & TUI
- Added the interactive rover outpost TUI for `ufo rover start`, with rover,
  operation, and event panes; keyboard navigation; detailed logs; uptime and
  clock display; and the full UFO name in the footer.
- Added `--headless` for supervisor-friendly rover logs while keeping the TUI
  as the default in interactive terminals.
- Added browser-approved rover enrollment: the CLI opens the Rovers page,
  pending approvals live in PostgreSQL, and the web modal can approve or deny
  with editable fleet, name, units, and tags.
- Added rover config streaming so Hub-side name, unit, tag, and fleet changes
  update a running rover without restarting.
- Added Hub/rover version gates, latest-release checks, `ufo rover upgrade`,
  and opt-in auto-upgrade/restart when the Hub requires a newer rover.
- Added per-rover unit updates from the web UI, bounded to 1-100 concurrent
  operations, with live semaphore resizing on the rover.
- Replaced `aws-lc-rs` with `rustls` using the `ring` crypto provider to cut
  cross-build friction and dependency weight.
- Added Grok Build pilot support and tightened pilot availability displays
  around enrolled/online rover counts.

### Web UI
- Reworked operation detail around a denser, floating reply composer that
  stays close to live pilot activity and queues replies while a pilot is
  already working.
- Added active-run and settled-run banners, telemetry dialogs that default to
  pilot-facing messages, and a preference to show all telemetry.
- Added worktree controls at fleet, mission, and operation levels, plus source
  handoff buttons when a source-capable rover is available.
- Added source-aware assignee controls, unavailable-pilot disabled states,
  cleaner rover/pilot counts, and compact icon-only pilot rows on the Rovers
  page.
- Added reusable asset display and preview components, Shiki-powered syntax
  highlighting, richer file-type icons, inline asset actions, and large
  preview dialogs.
- Added user name editing, fleet metadata editing, label editing, improved
  status icons, better Markdown asset rendering, and operation selection
  actions.

### Release, CI & Docs
- Added GitHub release automation for rover archives, checksums, Homebrew tap
  formula generation, and API/web container image publishing.
- Added production API and web Dockerfiles and release images for
  `linux/amd64` and `linux/arm64`.
- Expanded GitHub and GitLab CI so rover builds run as independent targets
  across macOS, FreeBSD, Linux, and Windows instead of a single default/cross
  split.
- Added release helper checks for version consistency, install-script syntax,
  Homebrew formula generation, and fresh-install smoke tests.
- Added the shell installer, Homebrew formula helper, production environment
  example, release labels, issue template config, and repository instructions
  for Codex, Claude, Gemini, GitHub Copilot, and AGENTS.md.
- Updated README and rover README for the public beta framing, architecture,
  assets, routines, source worktrees, storage backends, installation,
  supported platforms, and the new configuration surface.

## [0.2.1] - 2026-06-22

Cleanup release.

### Real-Time & Reliability
- Added tuning knobs for rover presence and run heartbeats.

### Protocol & Development
- Cleaned up the API contract and web types, and trimmed internal queries.

## [0.2.0] - 2026-06-22

Second public preview release.

### Operations Board
- Refined board filters with pilot-kind assignee filtering and queued/working
  active-work counts.
- Polished the operation detail layout, communications view, property rail,
  sidebar collapse, run controls, and date controls.
- Updated board and detail flows for the `/v1` Hub API paths.

### Pilots, Crews & Rovers
- Added built-in Antigravity, Cursor Agent, GitHub Copilot, Amp Code,
  OpenCode, OpenClaw, Hermes, Pi, Kimi, and Kiro pilots.
- Pilot management now uses built-in pilot kinds advertised by rovers; assign
  pilots by kind instead of creating/deleting stored pilot rows.
- Stored rover enrollments can start together on one host, and per-rover units
  let a rover run multiple operations concurrently.
- Added crew-captain orchestration: a captain can propose parallel
  sub-operations, UFO waits for them to settle, then reconvenes the captain to
  reconcile the results.
- Tightened dispatch with status reporting and safer no-rover blocking
  signals.
- Hardened crew administration: only owners/admins can create, rename, delete,
  or staff shared crews, and crew roles are limited to captain/member.

### API, Real-Time & Release
- Renamed public configuration to `UFO_HUB_*` / `UFO_ROVER_*`; update old
  `.env` files and rover launch commands from 0.1.x.
- Expanded the hand-maintained OpenAPI contract for the new board,
  relationships, label, reaction, rover, crew, signal, and run surfaces.
- Added API discovery via `/.well-known/api-catalog` and served the embedded
  OpenAPI contract at `/openapi.yaml`.
- Bumped preview app versions to 0.2.0 and refreshed Go, npm, and Cargo
  dependencies for release.

## [0.1.0] - 2026-06-15

First public preview release.

### Accounts & Tenancy
- Email/password auth with cookie sessions.
- **Fleets** (tenants) scope every entity; personal and group fleets.
- Members, roles (owner / admin / member), and email invitations.
- Owner/admin authorization protects membership, invitation, rover, and
  credential administration.

### Operations Board
- Default drag-and-drop **Kanban** board across statuses (backlog, todo,
  in_progress, in_review, done, blocked, canceled), plus **List** and
  **Lanes** views.
- Customizable columns and card properties, filters (priority / assignee /
  creator / label), and sorting.
- Operation detail: comment thread, priority, dates, labels, reactions,
  sub-operations, relationships (blocks / blocked-by / relates / duplicate),
  and linked pull requests.
- **Missions** group related operations; each mission key prefixes operation
  codes (e.g. mission key `MSJ` yields `MSJ-123`).
- Operation search, archiving, per-status counts, and per-mission counts.
- **Signals** surface review handoffs, failures, and requests for input to
  every human in the fleet.

### Pilots, Crews & Rovers
- **Pilots** are first-class entities backed by local AI CLIs, and are
  groupable into **crews** (pilots + humans).
- Humans can be assignees or crew members, but pilots are the ones that drive
  rovers.
- Assigning an operation to a pilot (or a crew with a pilot) auto-dispatches a
  run; runs execute in an isolated per-operation work directory and capture a
  git diff.
- **Rovers** are host-local runtimes enrolled through an enrollment-code to
  connection-token exchange, with available/full/offline status and per-rover
  connection-token revoke.
- A rover host can hold many fleet enrollments; pilot capability tags plus
  operation allow/deny tags are matched during dispatch.

### Conversations & Review Handoff
- Pilots work in resumable sessions, post results as comments, and hand off to
  **In Review** on success. A human reply resumes the session.
- Pilots can request input or set the operation status via reply sentinels.
- Per-run typed telemetry timeline, final messages, session metadata, and diff
  artifacts.

### Real-Time & Reliability
- WebSocket UI streaming and rover long-poll, both backed by PostgreSQL
  `LISTEN/NOTIFY`, operations, runs, rover presence/tags, and signals update
  without client polling or an extra broker.
- Orphaned-run lease sweeper requeues silent runs.
- Database-enforced single active run per operation prevents duplicate pilot
  dispatch.
- One-time enrollment codes are consumed atomically, and fleet owner changes
  preserve at least one owner.
- Stateless API instances coordinate migration and run accepts through
  PostgreSQL locking.

### Protocol & Development
- Hand-maintained OpenAPI contract for the API.
- Docker Compose development stack with automatically rebuilt API and web
  services; the rover runs on the host.
