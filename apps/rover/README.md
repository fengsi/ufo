# ufo-cli

[![GitHub](https://img.shields.io/badge/GitHub-fengsi%2Fufo-181717?style=flat-square&logo=github)](https://github.com/fengsi/ufo)
[![CI](https://img.shields.io/github/actions/workflow/status/fengsi/ufo/ci.yml?branch=main&style=flat-square&label=CI)](https://github.com/fengsi/ufo/actions/workflows/ci.yml)
[![crates.io](https://img.shields.io/crates/v/ufo-cli?style=flat-square)](https://crates.io/crates/ufo-cli)
[![License](https://img.shields.io/crates/l/ufo-cli?style=flat-square)](https://github.com/fengsi/ufo/blob/main/LICENSE)
[![Preview](https://img.shields.io/badge/status-preview-blue?style=flat-square)](https://github.com/fengsi/ufo/blob/main/CHANGELOG.md)
[![Rust](https://img.shields.io/badge/Rust-2024-B7410E?style=flat-square)](https://github.com/fengsi/ufo/blob/main/apps/rover/Cargo.toml)

`ufo-cli` is the host-side rover for UFO. It enrolls a local machine into a UFO
fleet, long-poll claims queued operations, runs the assigned pilot CLI
(`claude` or `codex`) in an isolated per-operation work directory, streams
telemetry back to the control plane, and uploads a `git diff` for review.

UFO is an MVP preview. APIs, configuration, database schema, and the rover
protocol may change before 1.0, and migration paths are not guaranteed.

## Install

```bash
cargo install ufo-cli
```

The rover is tested on macOS/Linux preview hosts. Windows is not validated yet.

## Enroll And Start

Start the UFO control plane first, then create an enrollment code from the
Rovers panel.

```bash
UFO_SERVER=http://localhost:8080 \
UFO_ENROLLMENT_CODE=<code> \
ufo rover enroll

ufo rover start
```

Registrations are stored in `~/.ufo/rovers.json`, keyed by rover id. A host can
hold registrations for multiple fleets or servers.

Useful commands:

```bash
ufo rover list
ufo rover remove <rover-id|prefix>
ufo rover remove --all
```

## Pilots And Tags

The rover auto-detects local pilot CLIs and reports capability tags such as
`pilot:claude`, `pilot:codex`, `os:macos`, and `arch:aarch64`. UFO only lets a
rover claim work when its tags match the queued operation.

You can add user dispatch tags during enrollment:

```bash
ufo rover enroll --tag gpu --tag region:moon
```

## Trust Boundary

Pilots run local CLIs with the privileges of the user that started the rover.
Use a dedicated low-privilege account, container, or isolated machine for rover
hosts you share with a fleet.

See [README.md](https://github.com/fengsi/ufo/blob/main/README.md) and
[SECURITY.md](https://github.com/fengsi/ufo/blob/main/SECURITY.md) for the
full control-plane setup and trust model.
