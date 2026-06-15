# Changelog

All notable changes to UFO are recorded here.

> **MVP preview compatibility notice:** releases before 1.0 do not provide API,
> database, configuration, rover-protocol, or behavioral compatibility
> guarantees. Changes may land without notice, and upgrading may require a
> database reset. A migration path is not guaranteed.

## [0.1.0] — 2026-06-15

First public preview release.

### Accounts & tenancy
- Email/password auth with cookie sessions.
- **Fleets** (tenants) scope every entity; personal and group fleets.
- Members, roles (owner / admin / member), and email invitations.
- Owner and manager authorization protects membership, invitation, rover, and
  credential administration.

### Operations board
- Default drag-and-drop **Kanban** board across statuses (backlog, todo,
  in_progress, in_review, done, blocked, cancelled), plus **List** and
  **Lanes** views.
- Customizable columns and card properties, filters (priority / assignee /
  creator / label), and sorting.
- Operation detail: comment thread, priority, dates, labels, reactions,
  sub-operations, relationships (blocks / blocked-by / relates / duplicate), and
  linked pull requests.
- **Missions** group related operations; each mission key prefixes operation
  codes (e.g. mission key `MSJ` yields `MSJ-123`).
- Operation search, archiving, per-status counts, and per-mission counts.
- **Signals** surface review handoffs, failures, and requests for input to every
  human in the fleet.

### Pilots, crews & rovers
- **Pilots** are first-class entities that currently run Claude or Codex locally,
  and are groupable into **crews** (pilots + humans).
- Humans can be assignees or crew members, but pilots are the ones that drive
  rovers.
- Assigning an operation to a pilot (or a crew with a pilot) auto-dispatches a
  run; runs execute in an isolated per-operation work dir and capture a git diff.
- **Rovers** are host-local runtimes enrolled through an enrollment-code to
  connection-token exchange, with online/busy/offline status and
  per-rover connection-token revoke.
- A rover host can hold many fleet registrations; pilot capability tags plus
  operation allow/deny tags are matched during dispatch.

### Conversations & review handoff
- Pilots work in resumable sessions, post results as comments, and hand off to
  **In Review** on success. A human reply resumes the session.
- Pilots can request input or set the operation status via reply sentinels.
- Per-run typed telemetry timeline, final messages, session metadata, and diff
  artifacts.

### Real-time & reliability
- WebSocket UI streaming and rover long-poll, both backed by PostgreSQL
  `LISTEN/NOTIFY` — operations, runs, rover presence/tags, and signals update
  without client polling or an extra broker.
- Orphaned-run lease sweeper requeues silent runs.
- Database-enforced single active run per operation prevents duplicate pilot
  dispatch.
- One-time enrollment codes are consumed atomically, and fleet owner changes
  preserve at least one owner.
- Stateless API instances coordinate migration and run claims through
  PostgreSQL locking.

### Protocol & development
- Hand-maintained OpenAPI contract for the control-plane, UI, and rover
  surfaces.
- Docker Compose development stack with automatically rebuilt API and web
  services; the rover runs on the host.
