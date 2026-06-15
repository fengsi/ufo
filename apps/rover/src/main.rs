//! UFO local rover.
//!
//! Long-poll claims queued runs from the control plane, runs each operation with
//! the assigned pilot (`claude` / `codex`), streams typed messages back, captures
//! the resulting `git diff`, and reports a terminal state.

use std::collections::HashMap;
use std::fs;
#[cfg(unix)]
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};
use std::process::Stdio;
use std::sync::Arc;
use std::time::Duration;

use anyhow::{Context, Result, anyhow};
use clap::{Parser, Subcommand};
use reqwest::{Client, StatusCode};
use serde::{Deserialize, Serialize};
use serde_json::json;
use tokio::io::{AsyncBufReadExt, BufReader};
use tokio::process::Command;
use tokio::sync::Semaphore;
use tokio::task::JoinSet;
use tokio::time::sleep;

/// Current local time in server-log style, e.g. `2026-06-07 23:15:42.123 -0700`.
fn ts() -> String {
    chrono::Local::now()
        .format("%Y-%m-%d %H:%M:%S%.3f %z")
        .to_string()
}

/// Print a timestamped log line.
macro_rules! logline {
    ($($arg:tt)*) => {{
        println!("{} {}", ts(), format!($($arg)*))
    }};
}

#[derive(Parser)]
#[command(name = "ufo", version, about = "UFO local rover")]
struct Cli {
    #[command(subcommand)]
    command: Commands,
}

#[derive(Subcommand)]
enum Commands {
    /// Rover controls.
    Rover {
        #[command(subcommand)]
        action: RoverAction,
    },
}

#[derive(Subcommand)]
enum RoverAction {
    /// Enroll a new rover registration (persisted locally under its server-assigned id).
    Enroll {
        #[arg(long, env = "UFO_SERVER", default_value = "http://localhost:8080")]
        server: String,
        /// Enrollment code (one-time or reusable) from the fleet's Rovers panel.
        #[arg(long, env = "UFO_ENROLLMENT_CODE")]
        enrollment_code: String,
        /// Rover name (defaults to the hostname).
        #[arg(long)]
        name: Option<String>,
        /// User tag(s) for dispatch matching; repeatable, e.g. --tag gpu --tag region:us.
        #[arg(long = "tag")]
        tags: Vec<String>,
    },
    /// List registered rovers on this host.
    List,
    /// Deregister a rover: delete it server-side and drop the local entry.
    Remove {
        /// Rover id or prefix to remove (see `rover list`). Omit with --all to remove every one.
        rover: Option<String>,
        /// Remove all registered rovers on this host.
        #[arg(long)]
        all: bool,
    },
    /// Run the claim/execute loop for one or (by default) all registered rovers.
    Start {
        /// Run just this rover id. Omit to run every registered rover.
        #[arg(long)]
        rover: Option<String>,
        /// Backoff after a claim *error* (server unreachable). Not a poll interval —
        /// claims long-poll and re-request immediately on idle.
        #[arg(long, default_value_t = 1)]
        poll_secs: u64,
        /// Max operations a single rover runs at once.
        #[arg(long, default_value_t = 1)]
        max_concurrent: usize,
        /// Outpost: this host's rover base dir (default ~/.ufo). Each op runs in an
        /// isolated `<outpost>/rovers/<rover-id>/operations/<operation-id>`.
        #[arg(long = "outpost", env = "UFO_OUTPOST")]
        outpost: Option<PathBuf>,
    },
}

/// Rover-facing view of a claimed run (matches the API's ClaimedRun).
#[derive(Debug, Deserialize)]
struct ClaimedRun {
    id: String,
    operation_id: String,
    #[allow(dead_code)]
    state: String,
    #[serde(default)]
    pilot: String,
    #[serde(default)]
    prompt: String,
    #[serde(default)]
    session_id: String,
}

#[tokio::main]
async fn main() -> Result<()> {
    let cli = Cli::parse();
    match cli.command {
        Commands::Rover { action } => match action {
            RoverAction::Enroll {
                server,
                enrollment_code,
                name,
                tags,
            } => cmd_enroll(&server, &enrollment_code, name, tags).await,
            RoverAction::List => cmd_list(),
            RoverAction::Remove { rover, all } => cmd_remove(rover, all).await,
            RoverAction::Start {
                rover,
                poll_secs,
                max_concurrent,
                outpost,
            } => cmd_start(rover, poll_secs, max_concurrent.max(1), outpost).await,
        },
    }
}

fn default_outpost() -> PathBuf {
    let home = std::env::var("HOME").unwrap_or_else(|_| ".".into());
    PathBuf::from(home).join(".ufo")
}

fn http_client() -> Client {
    // Timeout must exceed the server's claim long-poll (default 25s); other calls quick.
    Client::builder()
        .timeout(Duration::from_secs(60))
        .build()
        .expect("build http client")
}

/// Detected tags this host can advertise: pilot CLIs on PATH, plus os/arch.
async fn detect_auto_tags() -> Vec<String> {
    PilotCaps::detect().await.auto_tags()
}

async fn cmd_enroll(
    server: &str,
    enrollment_code: &str,
    name: Option<String>,
    tags: Vec<String>,
) -> Result<()> {
    let client = http_client();
    let nm = match name {
        Some(n) if !n.is_empty() => n,
        _ => default_name().await,
    };
    let auto = detect_auto_tags().await;
    let e = enroll(&client, server, enrollment_code, &nm, &tags, &auto).await?;
    save_entry(
        &e.id,
        &RoverEntry {
            server: server.to_string(),
            token: e.token.clone(),
            name: e.name.clone(),
            tags,
        },
    )?;
    logline!("enrolled rover '{}' ({}) on {server}", e.name, e.id);
    Ok(())
}

fn cmd_list() -> Result<()> {
    let cfg = load_config()?;
    if cfg.rovers.is_empty() {
        println!("no rovers registered — run `ufo rover enroll`");
        return Ok(());
    }
    for (rover_id, e) in &cfg.rovers {
        let tags = if e.tags.is_empty() {
            String::new()
        } else {
            format!(" [{}]", e.tags.join(", "))
        };
        println!("{rover_id}  {}  {}{tags}", e.name, e.server);
    }
    Ok(())
}

async fn cmd_remove(rover: Option<String>, all: bool) -> Result<()> {
    let mut cfg = load_config()?;
    let targets: Vec<String> = if all {
        cfg.rovers.keys().cloned().collect()
    } else {
        match rover {
            Some(u) => vec![resolve_rover_id(&cfg, &u)?],
            None => {
                return Err(anyhow!(
                    "pass a rover id/prefix (see `rover list`) or --all"
                ));
            }
        }
    };
    if targets.is_empty() {
        println!("no rovers registered");
        return Ok(());
    }
    let client = http_client();
    for u in targets {
        if let Some(e) = cfg.rovers.remove(&u) {
            // Best-effort server-side deregister (ignore if unreachable / already gone).
            let _ = client
                .delete(format!("{}/api/rover/me", e.server))
                .bearer_auth(&e.token)
                .send()
                .await;
            logline!("removed rover '{}' ({u})", e.name);
        }
    }
    write_config(&cfg)?;
    Ok(())
}

async fn cmd_start(
    rover: Option<String>,
    poll_secs: u64,
    max_concurrent: usize,
    outpost: Option<PathBuf>,
) -> Result<()> {
    let cfg = load_config()?;
    let selected: Vec<(String, RoverEntry)> = match rover {
        Some(key) => {
            let rover_id = resolve_rover_id(&cfg, &key)?;
            let e = cfg.rovers.get(&rover_id).expect("resolved").clone();
            vec![(rover_id, e)]
        }
        None => cfg.rovers.into_iter().collect(),
    };
    if selected.is_empty() {
        return Err(anyhow!("no rovers registered — run `ufo rover enroll`"));
    }
    let base = outpost.unwrap_or_else(default_outpost);
    fs::create_dir_all(&base)?;
    logline!("outpost: {}", base.display());

    // One async task per registration; a long run on one rover never stalls another.
    let mut set = JoinSet::new();
    for (rover_id, entry) in selected {
        let base = base.clone();
        set.spawn(async move {
            if let Err(e) = rover_loop(&rover_id, &entry, &base, poll_secs, max_concurrent).await {
                eprintln!("rover {rover_id} loop exited: {e:#}");
            }
        });
    }
    while set.join_next().await.is_some() {}
    Ok(())
}

/// One rover's claim/execute loop.
async fn rover_loop(
    rover_id: &str,
    entry: &RoverEntry,
    base: &Path,
    poll_secs: u64,
    max_concurrent: usize,
) -> Result<()> {
    let client = http_client();
    let server = entry.server.clone();
    let token = entry.token.clone();

    let op_base = base.join("rovers").join(rover_id).join("operations");
    fs::create_dir_all(&op_base)?;
    let sem = Arc::new(Semaphore::new(max_concurrent));
    let mut last_auto_tags = Vec::new();
    logline!(
        "rover {} ({rover_id}) started — long-polling {server} (max_concurrent={max_concurrent})",
        entry.name
    );

    loop {
        // Acquire a slot *before* claiming, so we never hold more work than we can run.
        let permit = sem.clone().acquire_owned().await.expect("semaphore");
        let caps = PilotCaps::detect().await;
        let auto_tags = caps.auto_tags();
        if auto_tags != last_auto_tags {
            logline!(
                "rover {} ({rover_id}) auto-tags: {}",
                entry.name,
                auto_tags.join(", ")
            );
            last_auto_tags = auto_tags.clone();
        }
        let _ = refresh_auto_tags(&client, &server, &token, &auto_tags).await;

        match claim(&client, &server, &token).await {
            Ok(Some(run)) => {
                let client = client.clone();
                let server = server.clone();
                let token = token.clone();
                let op_dir = op_base.join(&run.operation_id);
                tokio::spawn(async move {
                    let _permit = permit; // released when this run finishes
                    run_one(&client, &server, &token, &run, &op_dir, caps).await;
                });
            }
            Ok(None) => drop(permit),
            Err(e) if e.downcast_ref::<InvalidRoverToken>().is_some() => {
                drop(permit);
                return Err(anyhow!(
                    "stored connection token rejected by {server} — re-enroll this rover"
                ));
            }
            Err(e) => {
                drop(permit);
                eprintln!("[{rover_id}] claim error: {e:#}");
                sleep(Duration::from_secs(poll_secs)).await;
            }
        }
    }
}

/// Prepare the operation work tree and execute the run.
async fn run_one(
    client: &Client,
    server: &str,
    token: &str,
    run: &ClaimedRun,
    op_dir: &Path,
    caps: PilotCaps,
) {
    match ensure_workdir(op_dir).await {
        Ok(()) => {
            logline!(
                "claimed run {} (operation {}) in {}",
                &run.id,
                run.operation_id,
                op_dir.display()
            );
            if let Err(e) = execute_run(client, server, token, run, op_dir, caps).await {
                if e.downcast_ref::<RunLeaseLost>().is_some() {
                    eprintln!("run {} stopped: lease lost", run.id);
                } else {
                    eprintln!("run {} errored: {e:#}", run.id);
                    let _ = set_state(client, server, token, &run.id, "failed").await;
                }
            }
        }
        Err(e) => {
            eprintln!("work tree for run {} failed: {e:#}", run.id);
            let _ = set_state(client, server, token, &run.id, "failed").await;
        }
    }
}

/// Ensure an operation's isolated work dir exists and is git-init'd (one seed
/// commit so later diffs are meaningful). Idempotent — reused across the
/// operation's runs.
async fn ensure_workdir(path: &Path) -> Result<()> {
    fs::create_dir_all(path)?;
    if !path.join(".git").exists() {
        git(path, &["init", "-q", "-b", "main"]).await?;
        git(path, &["config", "user.email", "rover@ufo.local"]).await?;
        git(path, &["config", "user.name", "UFO Rover"]).await?;
        fs::write(path.join(".ufo-workdir"), "UFO operation work dir\n")?;
        git(path, &["add", "."]).await?;
        git(path, &["commit", "-q", "-m", "init operation work dir"]).await?;
    }
    Ok(())
}

/// Run a git command in `dir`, returning stdout (errors on non-zero exit).
async fn git(dir: &Path, args: &[&str]) -> Result<String> {
    let out = Command::new("git")
        .arg("-C")
        .arg(dir)
        .args(args)
        .output()
        .await?;
    if !out.status.success() {
        return Err(anyhow!(
            "git {:?} failed: {}",
            args,
            String::from_utf8_lossy(&out.stderr).trim()
        ));
    }
    Ok(String::from_utf8_lossy(&out.stdout).to_string())
}

#[derive(Debug)]
struct InvalidRoverToken;

impl std::fmt::Display for InvalidRoverToken {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("invalid connection token")
    }
}

impl std::error::Error for InvalidRoverToken {}

#[derive(Debug)]
struct RunLeaseLost;

impl std::fmt::Display for RunLeaseLost {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("run lease lost")
    }
}

impl std::error::Error for RunLeaseLost {}

/// Claim the oldest queued run, if any.
async fn claim(client: &Client, server: &str, token: &str) -> Result<Option<ClaimedRun>> {
    let resp = client
        .post(format!("{server}/api/rover/runs/claim"))
        .bearer_auth(token)
        .send()
        .await?;

    match resp.status() {
        StatusCode::NO_CONTENT => Ok(None),
        StatusCode::OK => Ok(Some(resp.json::<ClaimedRun>().await?)),
        StatusCode::UNAUTHORIZED => Err(InvalidRoverToken.into()),
        s => Err(anyhow!("claim returned {s}")),
    }
}

/// Pilot CLI executables available on this host.
#[derive(Clone)]
struct PilotCaps {
    claude: Option<PathBuf>,
    codex: Option<PathBuf>,
}

impl PilotCaps {
    async fn detect() -> Self {
        Self {
            claude: resolve_cli("claude").await,
            codex: resolve_cli("codex").await,
        }
    }

    fn auto_tags(&self) -> Vec<String> {
        let mut t = vec![
            format!("os:{}", std::env::consts::OS),
            format!("arch:{}", std::env::consts::ARCH),
        ];
        if self.claude.is_some() {
            t.push("pilot:claude".to_string());
        }
        if self.codex.is_some() {
            t.push("pilot:codex".to_string());
        }
        t
    }
}

/// Resolve `<name>` to the executable this rover will invoke.
async fn resolve_cli(name: &str) -> Option<PathBuf> {
    if let Some(path) = cli_on_path(name) {
        return Some(path);
    }

    Command::new(name)
        .arg("--version")
        .output()
        .await
        .ok()
        .filter(|o| o.status.success())
        .map(|_| PathBuf::from(name))
}

fn cli_on_path(name: &str) -> Option<PathBuf> {
    cli_search_dirs()
        .iter()
        .map(|dir| dir.join(name))
        .find(|path| is_executable_file(path))
}

fn cli_search_dirs() -> Vec<PathBuf> {
    let mut dirs = Vec::new();
    if let Some(path) = std::env::var_os("PATH") {
        dirs.extend(std::env::split_paths(&path));
    }
    if let Some(home) = std::env::var_os("HOME") {
        let home = PathBuf::from(home);
        dirs.push(home.join(".local/bin"));
        dirs.push(home.join(".cargo/bin"));
    }
    dirs.extend([
        PathBuf::from("/opt/homebrew/bin"),
        PathBuf::from("/usr/local/bin"),
        PathBuf::from("/usr/bin"),
        PathBuf::from("/bin"),
    ]);
    let mut out = Vec::with_capacity(dirs.len());
    for dir in dirs {
        if !out.contains(&dir) {
            out.push(dir);
        }
    }
    out
}

#[cfg(unix)]
fn is_executable_file(path: &Path) -> bool {
    fs::metadata(path)
        .map(|meta| meta.is_file() && meta.permissions().mode() & 0o111 != 0)
        .unwrap_or(false)
}

#[cfg(not(unix))]
fn is_executable_file(path: &Path) -> bool {
    fs::metadata(path)
        .map(|meta| meta.is_file())
        .unwrap_or(false)
}

/// Block a run because the pilot's CLI isn't installed on this rover.
async fn block_no_cli(
    client: &Client,
    server: &str,
    token: &str,
    run_id: &str,
    name: &str,
) -> Result<()> {
    append_event(
        client,
        server,
        token,
        run_id,
        "error",
        &format!("{name} CLI not available on this rover"),
    )
    .await?;
    set_state(client, server, token, run_id, "blocked").await?;
    logline!("run {run_id} -> blocked (no {name})");
    Ok(())
}

#[allow(clippy::too_many_arguments)]
async fn log_pilot_start(
    client: &Client,
    server: &str,
    token: &str,
    run_id: &str,
    name: &str,
    op_dir: &Path,
    prompt: &str,
    resume: bool,
) -> Result<()> {
    let verb = if resume { "resume" } else { name };
    append_event(
        client,
        server,
        token,
        run_id,
        "log",
        &format!("{verb} in {}: {}", op_dir.display(), first_line(prompt)),
    )
    .await
}

/// Sentinel a pilot appends when it needs a human to answer before it can
/// continue. The control plane turns this into an "input requested" signal.
const NEEDS_INPUT_SENTINEL: &str = "@@UFO_NEEDS_INPUT@@";
const NEEDS_INPUT_INSTRUCTION: &str = "If you cannot proceed without a decision or information only a human can provide, do not guess: end your reply with a line containing exactly @@UFO_NEEDS_INPUT@@ followed by your question.";
// The pilot may set the operation's status by emitting @@UFO_STATUS:<status>@@.
const STATUS_PREFIX: &str = "@@UFO_STATUS:";
const STATUS_INSTRUCTION: &str = "By default, finishing leaves the operation In Review for a human. To set a different outcome, end your reply with a line containing exactly @@UFO_STATUS:<status>@@ where <status> is one of in_review, done, blocked, cancelled.";

/// Extract a pilot-requested status from @@UFO_STATUS:x@@, if present.
fn parse_status(msg: &str) -> Option<String> {
    let i = msg.find(STATUS_PREFIX)?;
    let rest = &msg[i + STATUS_PREFIX.len()..];
    let end = rest.find("@@")?;
    Some(rest[..end].trim().to_string())
}

/// Remove a @@UFO_STATUS:x@@ marker from the message the human sees.
fn strip_status(msg: &str) -> String {
    let Some(i) = msg.find(STATUS_PREFIX) else {
        return msg.to_string();
    };
    let rest = &msg[i + STATUS_PREFIX.len()..];
    match rest.find("@@") {
        Some(end) => format!("{}{}", &msg[..i], &rest[end + 2..])
            .trim()
            .to_string(),
        None => msg.to_string(),
    }
}

/// Run a claimed operation with its assigned pilot, then capture a diff.
async fn execute_run(
    client: &Client,
    server: &str,
    token: &str,
    run: &ClaimedRun,
    op_dir: &Path,
    caps: PilotCaps,
) -> Result<()> {
    let pilot = run.pilot.as_str();

    // The operation title + body, with the needs-input + status protocols appended.
    let prompt = if run.prompt.trim().is_empty() {
        "Describe this repository in a new file SUMMARY.md.".to_string()
    } else {
        run.prompt.clone()
    };
    let cli_prompt = format!("{prompt}\n\n{NEEDS_INPUT_INSTRUCTION}\n{STATUS_INSTRUCTION}");

    let resume = !run.session_id.is_empty();

    // claude/codex stream JSONL so we can capture the session id + final message.
    let (mut cmd, is_cli) = match pilot {
        "claude" => {
            let Some(claude) = caps.claude.as_ref() else {
                return block_no_cli(client, server, token, &run.id, "claude").await;
            };
            let mut args: Vec<&str> = vec![
                "-p",
                &cli_prompt,
                "--output-format",
                "stream-json",
                "--verbose",
                "--permission-mode",
                "bypassPermissions",
            ];
            if resume {
                args.push("--resume");
                args.push(&run.session_id);
            }
            log_pilot_start(
                client, server, token, &run.id, "claude", op_dir, &prompt, resume,
            )
            .await?;
            let mut c = Command::new(claude);
            c.args(&args).current_dir(op_dir).stdin(Stdio::null());
            (c, true)
        }
        "codex" => {
            let Some(codex) = caps.codex.as_ref() else {
                return block_no_cli(client, server, token, &run.id, "codex").await;
            };
            // `codex exec` takes -s/--sandbox; `codex exec resume` rejects it and
            // wants the equivalent config override (-c sandbox_mode=…) instead.
            let args: Vec<&str> = if resume {
                vec![
                    "exec",
                    "resume",
                    &run.session_id,
                    "-c",
                    "sandbox_mode=workspace-write",
                    "--skip-git-repo-check",
                    "--json",
                    &cli_prompt,
                ]
            } else {
                vec![
                    "exec",
                    "-s",
                    "workspace-write",
                    "--skip-git-repo-check",
                    "--json",
                    &cli_prompt,
                ]
            };
            log_pilot_start(
                client, server, token, &run.id, "codex", op_dir, &prompt, resume,
            )
            .await?;
            let mut c = Command::new(codex);
            c.args(&args).current_dir(op_dir).stdin(Stdio::null());
            (c, true)
        }
        other => {
            append_event(
                client,
                server,
                token,
                &run.id,
                "error",
                &format!("unsupported pilot '{other}' (only claude/codex)"),
            )
            .await?;
            set_state(client, server, token, &run.id, "blocked").await?;
            return Ok(());
        }
    };

    set_state(client, server, token, &run.id, "running").await?;

    let pilot_run = async {
        if is_cli {
            run_streaming_json(client, server, token, &run.id, &mut cmd).await
        } else {
            let st = run_streaming(client, server, token, &run.id, &mut cmd).await?;
            Ok((st, String::new(), String::new()))
        }
    };
    tokio::pin!(pilot_run);
    let lease = heartbeat(client, server, token, &run.id);
    tokio::pin!(lease);

    let (status, session, mut message) = tokio::select! {
        result = &mut pilot_run => result?,
        result = &mut lease => {
            result?;
            return Err(anyhow!("heartbeat stopped unexpectedly"));
        }
    };

    // Sentinels are protocol markers, not part of the human-visible comment.
    let needs_input = message.contains(NEEDS_INPUT_SENTINEL);
    if needs_input {
        message = message.replace(NEEDS_INPUT_SENTINEL, "").trim().to_string();
    }
    let op_status = parse_status(&message).unwrap_or_default();
    if !op_status.is_empty() {
        message = strip_status(&message);
    }

    // Capture the working-tree diff (include untracked via intent-to-add).
    let _ = git(op_dir, &["add", "-N", "."]).await;
    let diff = git(op_dir, &["diff"]).await.unwrap_or_default();
    let content = if diff.trim().is_empty() {
        "(no changes)".to_string()
    } else {
        diff
    };
    upload_artifact(client, server, token, &run.id, "diff", "git.diff", &content).await?;

    // Record the session + post the pilot's final message as a comment.
    if is_cli {
        post_result(
            client,
            server,
            token,
            &run.id,
            &session,
            &message,
            needs_input,
            &op_status,
        )
        .await?;
    }

    let final_state = if status.success() {
        "succeeded"
    } else {
        "failed"
    };
    append_event(
        client,
        server,
        token,
        &run.id,
        "result",
        &format!("pilot exited with {:?}", status.code()),
    )
    .await?;
    set_state(client, server, token, &run.id, final_state).await?;
    logline!("run {} -> {}", &run.id, final_state);
    Ok(())
}

fn first_line(s: &str) -> &str {
    s.lines().next().unwrap_or("")
}

/// Renew the run's lease every 5s, returning if the server says ownership was lost.
async fn heartbeat(client: &Client, server: &str, token: &str, run_id: &str) -> Result<()> {
    loop {
        sleep(Duration::from_secs(5)).await;
        match client
            .post(format!("{server}/api/rover/runs/{run_id}/heartbeat"))
            .bearer_auth(token)
            .send()
            .await
        {
            Ok(resp) if resp.status().is_success() => {}
            Ok(resp)
                if matches!(
                    resp.status(),
                    StatusCode::NOT_FOUND | StatusCode::UNAUTHORIZED
                ) =>
            {
                return Err(RunLeaseLost.into());
            }
            Ok(resp) => logline!("heartbeat run {run_id} -> {}", resp.status()),
            Err(e) => logline!("heartbeat run {run_id} failed: {e}"),
        }
    }
}

/// Spawn the command (stdout+stderr piped), stream both as log events, and wait.
async fn run_streaming(
    client: &Client,
    server: &str,
    token: &str,
    run_id: &str,
    cmd: &mut Command,
) -> Result<std::process::ExitStatus> {
    cmd.stdout(Stdio::piped()).stderr(Stdio::piped());
    cmd.kill_on_drop(true);
    let mut child = cmd.spawn()?;
    let stdout = child.stdout.take().ok_or_else(|| anyhow!("no stdout"))?;
    let stderr = child.stderr.take().ok_or_else(|| anyhow!("no stderr"))?;

    // Drain stderr concurrently so a full pipe can't deadlock the child.
    let err_task = {
        let client = client.clone();
        let server = server.to_string();
        let token = token.to_string();
        let run_id = run_id.to_string();
        tokio::spawn(async move {
            let mut lines = BufReader::new(stderr).lines();
            while let Ok(Some(line)) = lines.next_line().await {
                let _ = client
                    .post(format!("{server}/api/rover/runs/{run_id}/events"))
                    .bearer_auth(&token)
                    .json(&json!({ "kind": "log", "message": line }))
                    .send()
                    .await;
            }
        })
    };

    // Shell output is the transcript: stream each stdout line as a text message.
    let mut seq: i64 = 0;
    let mut lines = BufReader::new(stdout).lines();
    while let Some(line) = lines.next_line().await? {
        logline!("[run {run_id}] {line}");
        post_message(
            client,
            server,
            token,
            run_id,
            &mut seq,
            "text",
            None,
            Some(&line),
            None,
            None,
        )
        .await?;
    }

    let status = child.wait().await?;
    let _ = err_task.await;
    Ok(status)
}

/// Like run_streaming but parses JSONL from claude (stream-json) / codex (--json):
/// streams readable lines as log events and returns (status, session_id, message).
async fn run_streaming_json(
    client: &Client,
    server: &str,
    token: &str,
    run_id: &str,
    cmd: &mut Command,
) -> Result<(std::process::ExitStatus, String, String)> {
    cmd.stdout(Stdio::piped()).stderr(Stdio::piped());
    cmd.kill_on_drop(true);
    let mut child = cmd.spawn()?;
    let stdout = child.stdout.take().ok_or_else(|| anyhow!("no stdout"))?;
    let stderr = child.stderr.take().ok_or_else(|| anyhow!("no stderr"))?;

    let err_task = {
        let client = client.clone();
        let server = server.to_string();
        let token = token.to_string();
        let run_id = run_id.to_string();
        tokio::spawn(async move {
            let mut lines = BufReader::new(stderr).lines();
            while let Ok(Some(line)) = lines.next_line().await {
                let _ = client
                    .post(format!("{server}/api/rover/runs/{run_id}/events"))
                    .bearer_auth(&token)
                    .json(&json!({ "kind": "log", "message": line }))
                    .send()
                    .await;
            }
        })
    };

    let mut session = String::new();
    let mut message = String::new();
    let mut seq: i64 = 0;
    let mut lines = BufReader::new(stdout).lines();
    while let Some(line) = lines.next_line().await? {
        let trimmed = line.trim();
        if trimmed.is_empty() {
            continue;
        }
        let v: serde_json::Value = match serde_json::from_str(trimmed) {
            Ok(v) => v,
            Err(_) => {
                post_message(
                    client,
                    server,
                    token,
                    run_id,
                    &mut seq,
                    "text",
                    None,
                    Some(trimmed),
                    None,
                    None,
                )
                .await?;
                continue;
            }
        };
        if let Some(s) = v.get("session_id").and_then(|x| x.as_str())
            && !s.is_empty()
        {
            session = s.to_string();
        }
        if v.get("type").and_then(|x| x.as_str()) == Some("thread.started")
            && let Some(s) = v.get("thread_id").and_then(|x| x.as_str())
        {
            session = s.to_string();
        }
        process_event(client, server, token, run_id, &mut seq, &v, &mut message).await?;
    }

    let status = child.wait().await?;
    let _ = err_task.await;
    Ok((status, session, message))
}

/// Translate one claude/codex JSON event into transcript messages.
async fn process_event(
    client: &Client,
    server: &str,
    token: &str,
    run_id: &str,
    seq: &mut i64,
    v: &serde_json::Value,
    message: &mut String,
) -> Result<()> {
    match v.get("type").and_then(|x| x.as_str()) {
        // claude: an assistant turn carries text / thinking / tool_use blocks.
        Some("assistant") => {
            if let Some(content) = v
                .get("message")
                .and_then(|m| m.get("content"))
                .and_then(|c| c.as_array())
            {
                for item in content {
                    match item.get("type").and_then(|x| x.as_str()) {
                        Some("text") => {
                            if let Some(t) = item.get("text").and_then(|x| x.as_str())
                                && !t.trim().is_empty()
                            {
                                post_message(
                                    client,
                                    server,
                                    token,
                                    run_id,
                                    seq,
                                    "text",
                                    None,
                                    Some(t),
                                    None,
                                    None,
                                )
                                .await?;
                            }
                        }
                        Some("thinking") => {
                            if let Some(t) = item.get("thinking").and_then(|x| x.as_str()) {
                                post_message(
                                    client,
                                    server,
                                    token,
                                    run_id,
                                    seq,
                                    "thinking",
                                    None,
                                    Some(t),
                                    None,
                                    None,
                                )
                                .await?;
                            }
                        }
                        Some("tool_use") => {
                            let name = item.get("name").and_then(|x| x.as_str()).unwrap_or("tool");
                            post_message(
                                client,
                                server,
                                token,
                                run_id,
                                seq,
                                "tool_use",
                                Some(name),
                                None,
                                item.get("input").cloned(),
                                None,
                            )
                            .await?;
                        }
                        _ => {}
                    }
                }
            }
        }
        // claude: tool results come back as a user message.
        Some("user") => {
            if let Some(content) = v
                .get("message")
                .and_then(|m| m.get("content"))
                .and_then(|c| c.as_array())
            {
                for item in content {
                    if item.get("type").and_then(|x| x.as_str()) == Some("tool_result") {
                        let out = tool_result_text(item.get("content"));
                        post_message(
                            client,
                            server,
                            token,
                            run_id,
                            seq,
                            "tool_result",
                            None,
                            None,
                            None,
                            Some(&out),
                        )
                        .await?;
                    }
                }
            }
        }
        Some("result") => {
            if let Some(t) = v.get("result").and_then(|x| x.as_str()) {
                *message = t.to_string();
            }
        }
        // codex: each item.completed is a step.
        Some("item.completed") => {
            if let Some(item) = v.get("item") {
                match item.get("type").and_then(|x| x.as_str()) {
                    // External Codex JSON uses this literal for assistant output.
                    Some("agent_message") => {
                        if let Some(t) = item.get("text").and_then(|x| x.as_str()) {
                            *message = t.to_string();
                            post_message(
                                client,
                                server,
                                token,
                                run_id,
                                seq,
                                "text",
                                None,
                                Some(t),
                                None,
                                None,
                            )
                            .await?;
                        }
                    }
                    Some("reasoning") => {
                        if let Some(t) = item.get("text").and_then(|x| x.as_str()) {
                            post_message(
                                client,
                                server,
                                token,
                                run_id,
                                seq,
                                "thinking",
                                None,
                                Some(t),
                                None,
                                None,
                            )
                            .await?;
                        }
                    }
                    Some("command_execution") => {
                        let cmd = item.get("command").and_then(|x| x.as_str()).unwrap_or("");
                        post_message(
                            client,
                            server,
                            token,
                            run_id,
                            seq,
                            "tool_use",
                            Some("shell"),
                            None,
                            Some(json!({ "command": cmd })),
                            None,
                        )
                        .await?;
                        if let Some(out) = item
                            .get("aggregated_output")
                            .or_else(|| item.get("output"))
                            .and_then(|x| x.as_str())
                            && !out.trim().is_empty()
                        {
                            post_message(
                                client,
                                server,
                                token,
                                run_id,
                                seq,
                                "tool_result",
                                Some("shell"),
                                None,
                                None,
                                Some(out),
                            )
                            .await?;
                        }
                    }
                    Some("file_change") => {
                        post_message(
                            client,
                            server,
                            token,
                            run_id,
                            seq,
                            "tool_use",
                            Some("edit"),
                            None,
                            item.get("changes").cloned(),
                            None,
                        )
                        .await?;
                    }
                    _ => {}
                }
            }
        }
        _ => {}
    }
    Ok(())
}

/// claude tool_result content is either a string or an array of text blocks.
fn tool_result_text(content: Option<&serde_json::Value>) -> String {
    match content {
        Some(serde_json::Value::String(s)) => s.clone(),
        Some(serde_json::Value::Array(arr)) => arr
            .iter()
            .filter_map(|b| b.get("text").and_then(|x| x.as_str()))
            .collect::<Vec<_>>()
            .join("\n"),
        _ => String::new(),
    }
}

/// Post one typed transcript message and advance the per-run sequence.
#[allow(clippy::too_many_arguments)]
async fn post_message(
    client: &Client,
    server: &str,
    token: &str,
    run_id: &str,
    seq: &mut i64,
    mtype: &str,
    tool: Option<&str>,
    content: Option<&str>,
    input: Option<serde_json::Value>,
    output: Option<&str>,
) -> Result<()> {
    let mut body = serde_json::Map::new();
    body.insert("seq".into(), json!(*seq));
    body.insert("type".into(), json!(mtype));
    if let Some(t) = tool {
        body.insert("tool".into(), json!(t));
    }
    if let Some(c) = content {
        body.insert("content".into(), json!(c));
    }
    if let Some(i) = input {
        body.insert("input".into(), i);
    }
    if let Some(o) = output {
        body.insert("output".into(), json!(o));
    }
    *seq += 1;
    let resp = client
        .post(format!("{server}/api/rover/runs/{run_id}/messages"))
        .bearer_auth(token)
        .json(&serde_json::Value::Object(body))
        .send()
        .await?;
    if matches!(
        resp.status(),
        StatusCode::NOT_FOUND | StatusCode::UNAUTHORIZED
    ) {
        return Err(RunLeaseLost.into());
    }
    if !resp.status().is_success() {
        logline!("message post run {run_id} -> {}", resp.status());
    }
    Ok(())
}

#[allow(clippy::too_many_arguments)]
async fn post_result(
    client: &Client,
    server: &str,
    token: &str,
    run_id: &str,
    session: &str,
    message: &str,
    needs_input: bool,
    op_status: &str,
) -> Result<()> {
    let resp = client
        .post(format!("{server}/api/rover/runs/{run_id}/result"))
        .bearer_auth(token)
        .json(&json!({ "session_id": session, "message": message, "needs_input": needs_input, "op_status": op_status }))
        .send()
        .await?;
    if !resp.status().is_success() {
        logline!("result post for run {run_id} -> {}", resp.status());
    }
    Ok(())
}

async fn set_state(
    client: &Client,
    server: &str,
    token: &str,
    run_id: &str,
    state: &str,
) -> Result<()> {
    let resp = client
        .post(format!("{server}/api/rover/runs/{run_id}/state"))
        .bearer_auth(token)
        .json(&json!({ "state": state }))
        .send()
        .await?;
    if !resp.status().is_success() {
        return Err(anyhow!("set_state {state} returned {}", resp.status()));
    }
    Ok(())
}

async fn append_event(
    client: &Client,
    server: &str,
    token: &str,
    run_id: &str,
    kind: &str,
    message: &str,
) -> Result<()> {
    let resp = client
        .post(format!("{server}/api/rover/runs/{run_id}/events"))
        .bearer_auth(token)
        .json(&json!({ "kind": kind, "message": message }))
        .send()
        .await?;
    if !resp.status().is_success() {
        return Err(anyhow!("append_event returned {}", resp.status()));
    }
    Ok(())
}

async fn upload_artifact(
    client: &Client,
    server: &str,
    token: &str,
    run_id: &str,
    kind: &str,
    name: &str,
    content: &str,
) -> Result<()> {
    let resp = client
        .post(format!("{server}/api/rover/runs/{run_id}/artifacts"))
        .bearer_auth(token)
        .json(&json!({ "kind": kind, "name": name, "content": content }))
        .send()
        .await?;
    if !resp.status().is_success() {
        return Err(anyhow!("upload_artifact returned {}", resp.status()));
    }
    Ok(())
}

// ---- registrations + local store (~/.ufo/rovers.json), keyed by rover id ----

#[derive(Serialize, Deserialize, Clone)]
struct RoverEntry {
    server: String,
    token: String,
    name: String,
    #[serde(default)]
    tags: Vec<String>, // user tags (auto-tags live server-side)
}

#[derive(Serialize, Deserialize, Default)]
struct RoverConfig {
    #[serde(default)]
    rovers: HashMap<String, RoverEntry>, // key = server-assigned rover id
}

#[derive(Debug, Deserialize)]
struct EnrollResp {
    token: String,
    id: String,
    name: String,
}

fn config_path() -> PathBuf {
    if let Ok(path) = std::env::var("UFO_CONFIG") {
        return PathBuf::from(path);
    }
    let home = std::env::var("HOME").unwrap_or_else(|_| ".".to_string());
    PathBuf::from(home).join(".ufo").join("rovers.json")
}

fn load_config() -> Result<RoverConfig> {
    match fs::read_to_string(config_path()) {
        Ok(s) => Ok(serde_json::from_str(&s).context("parse rover config")?),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(RoverConfig::default()),
        Err(e) => Err(e).context("read rover config"),
    }
}

/// Resolve a rover id or unambiguous prefix to a full rover id in the config.
fn resolve_rover_id(cfg: &RoverConfig, key: &str) -> Result<String> {
    if cfg.rovers.contains_key(key) {
        return Ok(key.to_string());
    }
    let matches: Vec<&String> = cfg.rovers.keys().filter(|k| k.starts_with(key)).collect();
    match matches.len() {
        1 => Ok(matches[0].clone()),
        0 => Err(anyhow!("no rover matching '{key}' — run `ufo rover list`")),
        _ => Err(anyhow!(
            "'{key}' is ambiguous ({} rovers match) — use more characters",
            matches.len()
        )),
    }
}

fn write_config(cfg: &RoverConfig) -> Result<()> {
    let path = config_path();
    if let Some(dir) = path.parent() {
        fs::create_dir_all(dir)?;
    }
    let tmp = path.with_extension("json.tmp");
    fs::write(&tmp, serde_json::to_vec_pretty(cfg)?)?;
    set_config_permissions(&tmp)?;
    fs::rename(tmp, path)?;
    Ok(())
}

#[cfg(unix)]
fn set_config_permissions(path: &Path) -> Result<()> {
    fs::set_permissions(path, fs::Permissions::from_mode(0o600))?;
    Ok(())
}

#[cfg(not(unix))]
fn set_config_permissions(_path: &Path) -> Result<()> {
    Ok(())
}

fn save_entry(rover_id: &str, entry: &RoverEntry) -> Result<()> {
    let mut cfg = load_config()?;
    cfg.rovers.insert(rover_id.to_string(), entry.clone());
    write_config(&cfg)
}

async fn default_name() -> String {
    let mut host = "rover".to_string();
    if let Ok(out) = Command::new("hostname").output().await {
        let h = String::from_utf8_lossy(&out.stdout).trim().to_string();
        if !h.is_empty() {
            host = h;
        }
    }
    // Suffix so multiple registrations on one host aren't indistinguishable;
    // the server id is the stable key, but a distinct name helps in the list/UI.
    let suffix = format!(
        "{:05x}",
        chrono::Local::now().timestamp_subsec_nanos() & 0xfffff
    );
    format!("{host}-{suffix}")
}

/// Refresh the rover's server-side auto-tags (idempotent; leaves user tags alone).
async fn refresh_auto_tags(
    client: &Client,
    server: &str,
    token: &str,
    tags: &[String],
) -> Result<()> {
    client
        .post(format!("{server}/api/rover/tags"))
        .bearer_auth(token)
        .json(&json!({ "tags": tags }))
        .send()
        .await?;
    Ok(())
}

/// Exchange an enrollment code for a per-rover connection token (advertising tags).
async fn enroll(
    client: &Client,
    server: &str,
    enrollment_code: &str,
    name: &str,
    tags: &[String],
    auto_tags: &[String],
) -> Result<EnrollResp> {
    let resp = client
        .post(format!("{server}/api/rover/enroll"))
        .bearer_auth(enrollment_code)
        .json(&json!({ "name": name, "tags": tags, "auto_tags": auto_tags }))
        .send()
        .await?;
    if !resp.status().is_success() {
        return Err(anyhow!("enroll returned {}", resp.status()));
    }
    Ok(resp.json().await?)
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Mutex;

    static ENV_LOCK: Mutex<()> = Mutex::new(());

    fn make_executable(path: &Path) -> Result<()> {
        #[cfg(unix)]
        {
            fs::set_permissions(path, fs::Permissions::from_mode(0o755))?;
        }
        Ok(())
    }

    fn set_env(key: &str, value: impl AsRef<std::ffi::OsStr>) {
        // SAFETY: env-mutating tests hold ENV_LOCK, so this test module never
        // mutates or reads these variables concurrently.
        unsafe { std::env::set_var(key, value) }
    }

    fn remove_env(key: &str) {
        // SAFETY: env-mutating tests hold ENV_LOCK, so this test module never
        // mutates or reads these variables concurrently.
        unsafe { std::env::remove_var(key) }
    }

    fn sample_entry(name: &str) -> RoverEntry {
        RoverEntry {
            server: "http://localhost:8080".to_string(),
            token: format!("{name}-token"),
            name: name.to_string(),
            tags: vec!["gpu".to_string(), "region:moon".to_string()],
        }
    }

    #[test]
    fn status_sentinel_is_parsed_and_stripped() {
        let msg = "Ready for review.\n@@UFO_STATUS:done@@";

        assert_eq!(parse_status(msg).as_deref(), Some("done"));
        assert_eq!(strip_status(msg), "Ready for review.");
    }

    #[test]
    fn status_sentinel_without_closer_is_left_alone() {
        let msg = "Almost there.\n@@UFO_STATUS:done";

        assert_eq!(parse_status(msg), None);
        assert_eq!(strip_status(msg), msg);
    }

    #[test]
    fn tool_result_text_accepts_string_and_text_blocks() {
        assert_eq!(
            tool_result_text(Some(&json!("plain output"))),
            "plain output"
        );
        assert_eq!(
            tool_result_text(Some(&json!([
                { "type": "text", "text": "first" },
                { "type": "image", "data": "ignored" },
                { "type": "text", "text": "second" }
            ]))),
            "first\nsecond"
        );
    }

    #[test]
    fn resolve_rover_id_requires_unambiguous_prefix() {
        let mut cfg = RoverConfig::default();
        cfg.rovers.insert("abc111".to_string(), sample_entry("one"));
        cfg.rovers.insert("abc222".to_string(), sample_entry("two"));
        cfg.rovers
            .insert("def333".to_string(), sample_entry("three"));

        assert_eq!(resolve_rover_id(&cfg, "def").unwrap(), "def333");
        assert!(resolve_rover_id(&cfg, "abc").is_err());
        assert!(resolve_rover_id(&cfg, "zzz").is_err());
    }

    #[test]
    fn cli_detection_uses_executables_on_path() {
        let _guard = ENV_LOCK.lock().unwrap();
        let base = std::env::temp_dir().join(format!(
            "ufo-rover-path-test-{}",
            chrono::Local::now()
                .timestamp_nanos_opt()
                .unwrap_or_default()
        ));
        fs::create_dir_all(&base).unwrap();
        let old_path = std::env::var_os("PATH");
        let old_home = std::env::var_os("HOME");
        let fake = base.join("fake-pilot");
        let home_bin = base.join(".local/bin");
        let home_fake = home_bin.join("home-pilot");

        let result = (|| -> Result<()> {
            fs::create_dir_all(&home_bin)?;
            fs::write(&fake, "#!/bin/sh\nexit 0\n")?;
            fs::write(&home_fake, "#!/bin/sh\nexit 0\n")?;
            make_executable(&fake)?;
            make_executable(&home_fake)?;
            set_env("PATH", &base);
            set_env("HOME", &base);

            assert_eq!(cli_on_path("fake-pilot").as_deref(), Some(fake.as_path()));
            assert_eq!(
                cli_on_path("home-pilot").as_deref(),
                Some(home_fake.as_path())
            );
            assert!(cli_on_path("missing-pilot").is_none());
            Ok(())
        })();

        match old_path {
            Some(path) => set_env("PATH", path),
            None => remove_env("PATH"),
        }
        match old_home {
            Some(home) => set_env("HOME", home),
            None => remove_env("HOME"),
        }
        let _ = fs::remove_dir_all(base);
        result.unwrap();
    }

    #[test]
    fn cli_detection_preserves_path_precedence() {
        let _guard = ENV_LOCK.lock().unwrap();
        let base = std::env::temp_dir().join(format!(
            "ufo-rover-path-order-test-{}",
            chrono::Local::now()
                .timestamp_nanos_opt()
                .unwrap_or_default()
        ));
        let first = base.join("first");
        let second = base.join("second");
        fs::create_dir_all(&first).unwrap();
        fs::create_dir_all(&second).unwrap();
        let old_path = std::env::var_os("PATH");

        let result = (|| -> Result<()> {
            for path in [first.join("pilot"), second.join("pilot")] {
                fs::write(&path, "#!/bin/sh\nexit 0\n")?;
                make_executable(&path)?;
            }
            let joined = std::env::join_paths([second.as_path(), first.as_path()])?;
            set_env("PATH", joined);

            let expected = second.join("pilot");
            assert_eq!(cli_on_path("pilot").as_deref(), Some(expected.as_path()));
            Ok(())
        })();

        match old_path {
            Some(path) => set_env("PATH", path),
            None => remove_env("PATH"),
        }
        let _ = fs::remove_dir_all(base);
        result.unwrap();
    }

    #[test]
    fn pilot_caps_emit_auto_tags_from_resolved_clis() {
        let caps = PilotCaps {
            claude: Some(PathBuf::from("/tmp/claude")),
            codex: None,
        };

        assert_eq!(
            caps.auto_tags(),
            vec![
                format!("os:{}", std::env::consts::OS),
                format!("arch:{}", std::env::consts::ARCH),
                "pilot:claude".to_string(),
            ]
        );
    }

    #[test]
    fn config_roundtrip_preserves_multiple_rover_registrations() {
        let _guard = ENV_LOCK.lock().unwrap();
        let base = std::env::temp_dir().join(format!(
            "ufo-rover-test-{}",
            chrono::Local::now()
                .timestamp_nanos_opt()
                .unwrap_or_default()
        ));
        let path = base.join("rovers.json");
        set_env("UFO_CONFIG", &path);

        let result = (|| -> Result<()> {
            save_entry("rover-a", &sample_entry("alpha"))?;
            save_entry("rover-b", &sample_entry("beta"))?;

            let cfg = load_config()?;
            assert_eq!(cfg.rovers.len(), 2);
            assert_eq!(cfg.rovers["rover-a"].token, "alpha-token");
            assert_eq!(cfg.rovers["rover-b"].token, "beta-token");

            #[cfg(unix)]
            {
                let mode = fs::metadata(&path)?.permissions().mode() & 0o777;
                assert_eq!(mode, 0o600);
            }
            Ok(())
        })();

        remove_env("UFO_CONFIG");
        let _ = fs::remove_dir_all(base);
        result.unwrap();
    }

    #[test]
    fn malformed_config_returns_an_error() {
        let _guard = ENV_LOCK.lock().unwrap();
        let base = std::env::temp_dir().join(format!(
            "ufo-rover-bad-config-{}",
            chrono::Local::now()
                .timestamp_nanos_opt()
                .unwrap_or_default()
        ));
        let path = base.join("rovers.json");
        fs::create_dir_all(&base).unwrap();
        fs::write(&path, "{not-json").unwrap();
        set_env("UFO_CONFIG", &path);

        let err = match load_config() {
            Ok(_) => panic!("malformed config should not be ignored"),
            Err(err) => err,
        };

        remove_env("UFO_CONFIG");
        let _ = fs::remove_dir_all(base);
        assert!(err.to_string().contains("parse rover config"));
    }
}
