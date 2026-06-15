# Security Policy

UFO is an MVP preview. Only the latest commit on the default branch is supported
for security fixes; compatibility is not guaranteed yet. See [README.md](README.md)
and [CHANGELOG.md](CHANGELOG.md) for the preview compatibility policy.

## Reporting a vulnerability

Please **do not open a public issue** for security problems. Report privately via
GitHub's **Security → Report a vulnerability** (private advisories) on
<https://github.com/fengsi/ufo>. We'll acknowledge and work with you on a fix and
disclosure timeline.

## Trust model (read before deploying)

UFO can run generated work on your machines, so the trust boundary matters:

- **A fleet is a trust boundary.** Any member of a fleet can cause **code
  execution on that fleet's connected rovers** by assigning an operation to a
  `Claude` / `Codex` pilot. The `Claude` adapter runs with
  `--permission-mode bypassPermissions` and `Codex` runs `exec` unattended.
  **Only invite people you trust with shell access to your rover hosts.**
- **Rovers run as the host user.** Pilot-driven commands use the privileges of
  the account that started the rover. Use a dedicated low-privilege user,
  container, or isolated machine. Per-operation work dirs under `~/.ufo`
  organize files; they are **not** a security sandbox.
- **Enrollment codes and connection tokens are bearer credentials.** Enrollment codes are
  shown **once** at creation; the listing only shows a masked prefix. Creating,
  listing, deleting enrollment codes and deleting rovers require an **owner or
  admin** role. Treat the values like passwords; revoke from the Rovers panel if
  leaked.
- **Local dev defaults are not production-safe.** `compose.yaml` ships throwaway
  PostgreSQL credentials and binds to localhost. Change them and put the API
  behind TLS before exposing it.
- **Do not disable TLS verification for pilot CLIs.** On hosts behind a
  TLS-inspecting proxy, install the proxy CA in the host trust store instead.

## Scope

The control plane is multi-instance-safe and fleet-scopes every query, but UFO
does **not** sandbox pilot execution — that is the operator's responsibility per
the trust model above.
