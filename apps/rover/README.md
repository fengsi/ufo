# ufo-cli

[![GitHub](https://img.shields.io/badge/GitHub-fengsi%2Fufo-181717?logo=github&style=for-the-badge)](https://github.com/fengsi/ufo)
[![Build](https://img.shields.io/github/actions/workflow/status/fengsi/ufo/ci.yml?logo=github&style=for-the-badge)](https://github.com/fengsi/ufo/actions/workflows/ci.yml)
[![crates.io](https://img.shields.io/crates/v/ufo-cli?style=for-the-badge)](https://crates.io/crates/ufo-cli)
[![License](https://img.shields.io/crates/l/ufo-cli?style=for-the-badge)](https://github.com/fengsi/ufo/blob/main/LICENSE)
[![Status](https://img.shields.io/badge/status-beta-blue?style=for-the-badge)](https://github.com/fengsi/ufo/blob/main/CHANGELOG.md)
[![Rust](https://img.shields.io/badge/Rust-2024-B7410E?logo=rust&style=for-the-badge)](https://github.com/fengsi/ufo/blob/main/apps/rover/Cargo.toml)

`ufo-cli` is the host-side rover for UFO. It enrolls a local machine into a
UFO fleet, long-poll claims queued operations, lets the assigned pilot drive
the rover in an isolated per-operation work directory, streams telemetry back
to the Hub, and keeps resulting diffs attached to the operation.

UFO is in public beta. Release notes call out upgrade caveats for each tagged
release, and APIs, configuration, database schema, storage paths, and the
rover protocol may still change before 1.0. Back up before upgrading,
especially when testing arbitrary source commits.

## Install

Shell installer (macOS, FreeBSD, Linux):

```bash
curl -fsSL https://getufo.dev/install.sh | sh
```

The installer puts `ufo` in `~/.local/bin` by default. Override with
`UFO_ROVER_INSTALL_DIR=/usr/local/bin`, or pin a release with:

```bash
curl -fsSL https://getufo.dev/install.sh | UFO_ROVER_VERSION=v0.3.1 sh
```

Homebrew (macOS, Linux):

```bash
brew install fengsi/ufo/ufo-cli
```

Windows:

- Download the Windows archive for your CPU from GitHub Releases and put
  `ufo.exe` on `PATH`.
- Or use Cargo:

  ```powershell
  cargo install ufo-cli
  ```

Cargo fallback:

```bash
cargo install ufo-cli
```

The rover CI runs on macOS, FreeBSD, Linux, and Windows. Other operating
systems may work, but are not tested yet.

## Enroll and start

Start the UFO Hub first, then enroll. The command opens the Rovers page, tells
you to sign in if needed, pick the fleet/name/units/tags in the approval
modal, waits until approval stores the rover token, then starts the rover.

First run:

```bash
ufo rover enroll --hub http://localhost:8080
```

Later runs:

```bash
ufo rover start
```

You can also pass the Hub with `UFO_HUB_URL=http://localhost:8080`.
`ufo rover start` uses the Hub stored during enrollment.

Enrollments are stored in `~/.ufo/rovers.json`, keyed by rover id. A host can
hold enrollments for multiple fleets or servers.

For code-based or non-browser enrollment, create an enrollment code from the
Rovers panel and pass it with `UFO_ROVER_ENROLLMENT_CODE=<code> ufo rover
enroll`.

`ufo rover enroll` and `ufo rover start` open the live rover TUI when stdout
is an interactive terminal. They still run the rover daemon loop: each
enrollment long-polls for work. Use `ufo rover enroll --headless` on first
run, or `ufo rover start --headless` later, for CI, launchd/systemd, or old
log-oriented output.

Use `ufo rover enroll --auto-upgrade`, `ufo rover start --auto-upgrade`, or
`UFO_ROVER_AUTO_UPGRADE=1` to install and restart automatically when the Hub
requires a newer rover. On Windows, update from the release archive or Cargo
instead.

Useful commands:

```bash
ufo rover list
ufo rover status
ufo rover remove <rover-id|prefix>
ufo rover remove --all
```

## Pilots and tags

The rover auto-detects local AI CLIs and reports capability tags for dispatch.
UFO only lets a rover claim work when its tags match the queued operation.

| UFO pilot name | CLI on PATH | Capability tag |
| --- | --- | --- |
| Claude Code | `claude` | `pilot:claude` |
| Codex | `codex` | `pilot:codex` |
| Antigravity | `agy` | `pilot:antigravity` |
| Grok Build | `grok` | `pilot:grok` |
| Cursor Agent | `cursor-agent` | `pilot:cursor` |
| GitHub Copilot | `copilot` | `pilot:copilot` |
| Amp Code | `amp` | `pilot:amp` |
| OpenCode | `opencode` | `pilot:opencode` |
| OpenClaw | `openclaw` | `pilot:openclaw` |
| Hermes | `hermes` | `pilot:hermes` |
| Pi | `pi` | `pilot:pi` |
| Kimi | `kimi` | `pilot:kimi` |
| Kiro | `kiro-cli` | `pilot:kiro` |

Rovers also report host tags such as `os:macos` and `arch:aarch64`.

You can add user dispatch tags during enrollment:

```bash
ufo rover enroll --tag gpu --tag region:moon
```

Set per-rover concurrency (1-100) in the web Rovers panel or during enrollment
with `ufo rover enroll --units N`. `ufo rover start --units N` and
`UFO_ROVER_UNITS=N` are only startup fallbacks until hub config is available.

## Trust boundary

Pilots run local CLIs with the privileges of the user that started the rover.
Use a dedicated low-privilege account, container, or isolated machine for
rover hosts you share with a fleet.

See [README.md](https://github.com/fengsi/ufo/blob/main/README.md) for the Hub
run guide and
[SECURITY.md](https://github.com/fengsi/ufo/blob/main/SECURITY.md) for the
trust model.

## Update and uninstall

`ufo rover enroll` and `ufo rover start` check the latest GitHub release and
print a warning when the local rover is behind. The check is best effort and
never blocks enrollment or startup if it fails.

Update with `ufo rover upgrade`, by rerunning the curl installer, `brew
upgrade ufo-cli`, or `cargo install ufo-cli --force`. On Windows, use the new
release archive or `cargo install ufo-cli --force`.

Remove the binary with `rm -f ~/.local/bin/ufo`, `brew uninstall ufo-cli`, or
`cargo uninstall ufo-cli`. Remove rover enrollments first with `ufo rover
remove --all`; local enrollment and outpost data live under `~/.ufo` unless
`UFO_ROVER_CONFIG` or `UFO_ROVER_OUTPOST` points elsewhere.

## Release artifacts

GitHub releases publish one rover archive and one SHA-256 file per target:

```text
ufo-aarch64-apple-darwin.tar.gz
ufo-x86_64-apple-darwin.tar.gz
ufo-x86_64-unknown-freebsd.tar.gz
ufo-aarch64-unknown-linux-gnu.tar.gz
ufo-x86_64-unknown-linux-gnu.tar.gz
ufo-aarch64-unknown-linux-musl.tar.gz
ufo-x86_64-unknown-linux-musl.tar.gz
ufo-aarch64-pc-windows-gnullvm.tar.gz
ufo-x86_64-pc-windows-gnullvm.tar.gz
ufo-aarch64-pc-windows-msvc.tar.gz
ufo-x86_64-pc-windows-msvc.tar.gz
```

Each archive contains the `ufo` binary (`ufo.exe` on Windows), license, and
rover README. The curl installer verifies the matching `.sha256` before
installing. Release assets are not signed yet; publish signature files beside
the checksums before documenting signature verification.
