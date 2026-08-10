# Security Policy

UFO is in public beta. Only the latest public release and the current default
branch are supported for security fixes. Compatibility is still pre-1.0:
release notes call out upgrade caveats for each tagged release, and APIs,
schema, configuration, storage paths, and the rover protocol may still change.
See [README.md](README.md) and [CHANGELOG.md](CHANGELOG.md) for the beta
compatibility policy.

## Reporting a vulnerability

Please **do not open a public issue** for security problems. Report privately
via GitHub's **Security → Report a vulnerability** (private advisories) on
<https://github.com/fengsi/ufo>. We'll acknowledge and work with you on a fix
and disclosure timeline.

## Trust model (read before deploying)

UFO can run generated work on your machines, so the trust boundary matters:

- **A fleet is a trust boundary.** Any member of a fleet can cause **code
  execution on that fleet's connected rovers** by assigning an operation to a
  pilot backed by a [supported AI CLI](apps/rover/README.md#pilots-and-tags).
  Pilots run unattended with broad local permissions.
  **Only invite people you trust with shell access to your rover hosts.**
- **Rovers run as the host user.** Pilot-driven commands use the privileges of
  the account that started the rover. Use a dedicated low-privilege user,
  container, or isolated machine. When started from a git checkout, the rover
  uses per-operation git worktrees under `~/.ufo` to avoid editing the running
  checkout directly. Non-ignored local changes are copied into each worktree;
  those worktrees are **not** a security sandbox.
- **Enrollment codes and connection tokens are bearer credentials.**
  Enrollment codes are shown **once** at creation; the listing only shows a
  masked prefix. Only an **owner or admin** can create, list, or delete
  enrollment codes, or delete rovers. Treat the values like passwords; revoke
  from the Rovers panel if leaked.
- **Browser-approved enrollment URLs are bearer approvals.** The code is
  carried in the URL fragment so it is not sent to the Hub during login
  redirects; the web app clears the fragment after capturing it. Anyone who
  sees the full URL before then can request that rover's enrollment. Web
  approvals are single-use and short-lived.
- **Local dev defaults are not production-safe.** `compose.yaml` ships
  throwaway PostgreSQL credentials and binds to localhost. Change them and put
  the API behind TLS before exposing it.
- **Do not disable TLS verification for pilots.** On hosts behind a
  TLS-inspecting proxy, install the proxy CA in the host trust store instead.

## Scope

The Hub is multi-instance-safe and fleet-scopes every query, but UFO does
**not** sandbox pilot execution, that is the host owner's responsibility per
the trust model above.
