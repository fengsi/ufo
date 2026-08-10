use super::*;
use std::sync::{Mutex, MutexGuard};

static ENV_LOCK: Mutex<()> = Mutex::new(());

fn env_lock() -> MutexGuard<'static, ()> {
    ENV_LOCK
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner())
}

async fn init_test_git_repo(path: &Path) -> Result<()> {
    git(path, &["init", "-q", "-b", "main"]).await?;
    git(path, &["config", "user.email", "rover@ufo.local"]).await?;
    git(path, &["config", "user.name", "UFO Rover"]).await?;
    git(path, &["config", "core.autocrlf", "false"]).await?;
    Ok(())
}

fn make_executable(_path: &Path) -> Result<()> {
    #[cfg(unix)]
    {
        fs::set_permissions(_path, fs::Permissions::from_mode(0o755))?;
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

fn sample_accepted_run(pilot: &str) -> AcceptedRun {
    AcceptedRun {
        id: "run".to_string(),
        operation_id: "operation".to_string(),
        operation_worktree_name: "UFO-1-operation".to_string(),
        operation_created_at: "2026-06-18T18:18:18Z".to_string(),
        worktree_enabled: true,
        worktree_base_ref: String::new(),
        worktree_fallback_ref: String::new(),
        worktree_refresh_ref: String::new(),
        worktree_base_sync: String::new(),
        auto_commit_branch: String::new(),
        status: "queued".to_string(),
        pilot: pilot.to_string(),
        prompt: String::new(),
        session_id: "session".to_string(),
        can_propose_sub_operations: false,
        reconcile: false,
        reconcile_imports: Vec::new(),
        assets: Vec::new(),
        skills: vec![],
    }
}

fn plain_ansi(value: &str) -> String {
    let mut out = String::new();
    let mut chars = value.chars();
    while let Some(ch) = chars.next() {
        if ch == '\x1b' {
            for ch in chars.by_ref() {
                if ch == 'm' {
                    break;
                }
            }
        } else {
            out.push(ch);
        }
    }
    out
}

#[test]
fn rover_headers_include_cli_version() {
    let headers = rover_headers();
    assert_eq!(
        headers
            .get(ROVER_VERSION_HEADER)
            .and_then(|v| v.to_str().ok()),
        Some(env!("CARGO_PKG_VERSION"))
    );
}

#[test]
fn redact_secrets_covers_github_and_gitlab_token_shapes() {
    let s = "token github_pat_ABC123 and ghp_XYZ and glpat-QQQ and gho_A ghs_B ghu_C ghr_D";
    let out = redact_secrets(s);
    assert!(out.contains("github_pat_***"), "{out}");
    assert!(out.contains("ghp_***"), "{out}");
    assert!(out.contains("glpat-***"), "{out}");
    assert!(out.contains("gho_***"), "{out}");
    assert!(out.contains("ghs_***"), "{out}");
    assert!(out.contains("ghu_***"), "{out}");
    assert!(out.contains("ghr_***"), "{out}");
    assert!(!out.contains("ABC123"), "{out}");
    assert!(!out.contains("XYZ"), "{out}");
    assert!(!out.contains("QQQ"), "{out}");

    let url = "https://x-access-token:sekrit@github.com/a/b.git";
    let red = redact_secrets(url);
    assert!(red.contains("***@"), "{red}");
    assert!(!red.contains("sekrit"), "{red}");
}

fn sample_forge_action(provider: &str, base_url: &str, repo: &str) -> forge::AcceptedForgeAction {
    forge::AcceptedForgeAction {
        id: "1".into(),
        kind: "push_head_branch".into(),
        provider: provider.into(),
        base_url: base_url.into(),
        repo: repo.into(),
        head_branch: "feat".into(),
        base_branch: "main".into(),
        commit_sha: String::new(),
        title: String::new(),
        body: String::new(),
        credential_kind: String::new(),
        credential_name: String::new(),
        checks_commands: vec![],
        checks_timeout_seconds: 0,
        ship_base_sync: String::new(),
        lease_seconds: 600,
        operation_id: String::new(),
        operation_worktree_name: String::new(),
        operation_created_at: String::new(),
    }
}

#[test]
fn forge_lease_renewal_times_out_before_stale_reaccept() {
    assert!(!forge::forge_lease_renewal_timed_out(
        Duration::from_secs(299),
        600
    ));
    assert!(forge::forge_lease_renewal_timed_out(
        Duration::from_secs(300),
        600
    ));
}

#[test]
fn forge_remote_url_is_credential_free_and_scheme_aware() {
    let gh = sample_forge_action("github", "https://api.github.com", "org/repo");
    let url = forge::remote_https_url(&gh).unwrap();
    assert_eq!(url, "https://x-access-token@github.com/org/repo.git");
    assert!(!url.contains("x-access-token:"), "{url}");

    let gl = sample_forge_action("gitlab", "http://gitlab.example/api/v4", "group/proj");
    let url = forge::remote_https_url(&gl).unwrap();
    assert_eq!(url, "http://oauth2@gitlab.example/group/proj.git");
    assert!(!url.contains("oauth2:"), "{url}");
}

#[test]
fn forge_discover_head_sha_matches_when_required() {
    assert!(forge::discover_head_sha_matches("", "abc"));
    assert!(forge::discover_head_sha_matches("ABC", "abc"));
    assert!(!forge::discover_head_sha_matches("abc", "abd"));
    assert!(!forge::discover_head_sha_matches("abc", ""));
}

#[test]
fn hub_protocol_uses_product_semver() {
    assert!(hub_protocol_supported("0.7.3"));
    assert!(hub_protocol_supported("0.8.0"));
    assert!(hub_protocol_supported("v0.7.3"));
    assert!(hub_protocol_supported("0.7.3+dev.abc1234"));
    assert!(!hub_protocol_supported("0.7.2"));
    assert!(!hub_protocol_supported("0.6.2"));
    assert!(!hub_protocol_supported("dev"));
    assert!(ensure_hub_protocol("0.7.3").is_ok());
    assert!(ensure_hub_protocol("dev").is_err());
}

#[test]
fn forge_provider_response_shapes() {
    let gh_list = serde_json::json!({
        "number": 12,
        "html_url": "https://github.com/fleet/mission/pull/12",
        "title": "operation ship",
        "base": { "ref": "orbit" },
        "head": { "ref": "operation/head", "sha": "abc123" }
    });
    let (base, head, sha) = forge::github_list_pr_identity(&gh_list);
    assert_eq!(base, "orbit");
    assert_eq!(head, "operation/head");
    assert_eq!(sha, "abc123");
    assert_eq!(
        forge::github_pr_mergeable(&serde_json::json!({"mergeable": true})),
        Some(true)
    );
    assert_eq!(
        forge::github_pr_mergeable(&serde_json::json!({"mergeable": null})),
        None
    );

    let gl_list = serde_json::json!({
        "iid": 7,
        "web_url": "https://gitlab.example/fleet/mission/-/merge_requests/7",
        "title": "operation ship",
        "target_branch": "orbit",
        "source_branch": "operation/head",
        "sha": "def456",
        "detailed_merge_status": "mergeable"
    });
    let (base, head, sha) = forge::gitlab_list_mr_identity(&gl_list);
    assert_eq!(base, "orbit");
    assert_eq!(head, "operation/head");
    assert_eq!(sha, "def456");
    assert_eq!(forge::gitlab_mr_mergeable(&gl_list), Some(true));
    assert_eq!(
        forge::gitlab_mr_mergeable(&serde_json::json!({"detailed_merge_status": "conflict"})),
        Some(false)
    );
}

#[test]
fn forge_percent_encode_branch_names() {
    assert_eq!(forge::percent_encode("feature/foo"), "feature%2Ffoo");
    assert_eq!(forge::percent_encode("safe-name_1"), "safe-name_1");
}

#[test]
fn enroll_response_values_seed_local_rover_entry() {
    let response = EnrollResp {
        token: "token".to_string(),
        id: "rover-id".to_string(),
        fleet_id: Some("fleet-id".to_string()),
        fleet_name: Some("Fleet".to_string()),
        name: "approved".to_string(),
        units: Some(3),
        tags: vec!["gpu".to_string()],
    };

    let entry = rover_entry_from_enroll("http://hub", &response, 1, vec!["cli-tag".to_string()]);

    assert_eq!(entry.name, "approved");
    assert_eq!(entry.units, 3);
    assert_eq!(entry.tags, vec!["gpu"]);

    let fallback = EnrollResp {
        units: None,
        tags: Vec::new(),
        ..response
    };
    let entry = rover_entry_from_enroll("http://hub", &fallback, 2, vec!["cli-tag".to_string()]);
    assert_eq!(entry.units, 2);
    assert_eq!(entry.tags, vec!["cli-tag"]);
}

#[test]
fn version_compare_accepts_release_tags() {
    assert_eq!(parse_version("v1.2.3"), Some([1, 2, 3]));
    assert_eq!(parse_version("1.2.3-beta.1"), Some([1, 2, 3]));
    assert_eq!(
        compare_versions("v0.3.1", "0.3.0"),
        Some(std::cmp::Ordering::Greater)
    );
    assert_eq!(
        compare_versions("0.3.0", "v0.3.0"),
        Some(std::cmp::Ordering::Equal)
    );
    assert_eq!(parse_version("dev"), None);
}

#[test]
fn rover_version_error_only_auto_upgrades_when_too_old() {
    let too_old = rover_version_error("accept", Some("9.0.0"), None, "");
    assert!(too_old.can_auto_upgrade);
    assert!(too_old.to_string().contains("9.0.0 or newer"));
    let err: anyhow::Error = too_old.into();
    assert!(should_auto_upgrade_error(&err, true));
    assert!(!should_auto_upgrade_error(&err, false));

    let too_new = rover_version_error("accept", Some("0.2.0"), Some("0.2.9"), "");
    assert!(!too_new.can_auto_upgrade);
    assert!(too_new.to_string().contains("between 0.2.0 and 0.2.9"));
    let err: anyhow::Error = too_new.into();
    assert!(!should_auto_upgrade_error(&err, true));
}

#[test]
fn mark_upgrade_required_stops_new_accepts() {
    let revoked = AtomicBool::new(false);
    let upgrade_required = AtomicBool::new(false);
    let can_auto_upgrade = AtomicBool::new(false);
    let err = rover_version_error("heartbeat", Some("9.0.0"), None, "");

    mark_upgrade_required(&err, &revoked, &upgrade_required, &can_auto_upgrade);

    assert!(revoked.load(Ordering::Relaxed));
    assert!(upgrade_required.load(Ordering::Relaxed));
    assert!(can_auto_upgrade.load(Ordering::Relaxed));
}

#[test]
fn web_enrollment_check_stops_for_terminal_decisions() {
    assert_eq!(
        response_error_message(
            StatusCode::UNAUTHORIZED,
            r#"{"error":"enrollment code expired"}"#
        ),
        "enrollment code expired"
    );
    assert_eq!(
        web_enrollment_check_error(StatusCode::UNAUTHORIZED, "invalid enrollment code"),
        None
    );
    assert_eq!(
        web_enrollment_check_error(StatusCode::UNAUTHORIZED, "enrollment pending"),
        None
    );
    assert!(
        web_enrollment_check_error(StatusCode::UNAUTHORIZED, "enrollment code expired").is_some()
    );
    assert!(web_enrollment_check_error(StatusCode::FORBIDDEN, "enrollment denied").is_some());
}

#[test]
fn update_hint_matches_platform_installer_support() {
    #[cfg(windows)]
    assert!(update_hint_for_current_install().contains("GitHub release archive"));
    #[cfg(not(windows))]
    {
        assert!(update_hint_for_exe(Path::new("/opt/ufo/bin/ufo")).contains("ufo rover upgrade"));
        assert_eq!(
            update_hint_for_exe(Path::new("/opt/homebrew/Cellar/ufo-cli/0.3.0/bin/ufo")),
            "Update with `brew upgrade fengsi/ufo/ufo-cli`."
        );
    }
}

#[test]
fn upgrade_env_keeps_only_requested_values() {
    assert!(upgrade_env(Some("  "), None).is_empty());
    assert_eq!(
        upgrade_env(Some(" v0.6.0 "), Some(Path::new("/opt/ufo"))),
        vec![
            ("UFO_ROVER_VERSION", "v0.6.0".to_string()),
            ("UFO_ROVER_INSTALL_DIR", "/opt/ufo".to_string())
        ]
    );
}

#[test]
#[cfg(not(windows))]
fn homebrew_version_override_requires_real_value() {
    assert!(!has_homebrew_version_override(None));
    assert!(!has_homebrew_version_override(Some("  ")));
    assert!(has_homebrew_version_override(Some("v0.6.0")));
}

#[test]
fn auto_upgrade_installs_next_to_current_binary() {
    assert_eq!(
        install_dir_for_exe(Path::new("/opt/ufo/bin/ufo")).unwrap(),
        PathBuf::from("/opt/ufo/bin")
    );
}

#[test]
#[cfg(not(windows))]
fn detects_homebrew_cellar_install() {
    assert!(is_homebrew_install(Path::new(
        "/opt/homebrew/Cellar/ufo-cli/0.3.0/bin/ufo"
    )));
    assert!(is_homebrew_install(Path::new(
        "/home/linuxbrew/.linuxbrew/Cellar/ufo-cli/0.3.0/bin/ufo"
    )));
    assert!(!is_homebrew_install(Path::new("/opt/ufo/bin/ufo")));
}

#[test]
#[cfg(unix)]
fn homebrew_restart_uses_path_shim() {
    assert_eq!(
        restart_program_for_exe(Path::new("/opt/homebrew/Cellar/ufo-cli/0.3.0/bin/ufo")),
        PathBuf::from("ufo")
    );
    assert_eq!(
        restart_program_for_exe(Path::new("/opt/ufo/bin/ufo")),
        PathBuf::from("/opt/ufo/bin/ufo")
    );
}

#[test]
fn remember_first_rover_error_keeps_first_failure() {
    let mut first = None;
    remember_first_rover_error(&mut first, Ok(Err(anyhow!("first failure"))));
    remember_first_rover_error(&mut first, Ok(Err(anyhow!("second failure"))));
    assert_eq!(first.unwrap().to_string(), "first failure");

    let mut joined = None;
    let join_error = tokio::runtime::Runtime::new().unwrap().block_on(async {
        let task = tokio::spawn(async { std::future::pending::<()>().await });
        task.abort();
        task.await.unwrap_err()
    });
    remember_first_rover_error(&mut joined, Err(join_error));
    assert!(joined.unwrap().to_string().contains("rover task failed"));
}

#[test]
fn hub_asset_url_accepts_v1_paths() {
    assert_eq!(
        hub_asset_url("http://hub/", "/v1/assets/a/file"),
        "http://hub/v1/assets/a/file"
    );
    assert_eq!(
        hub_asset_url("http://hub", "/api/v1/assets/a/file"),
        "http://hub/v1/assets/a/file"
    );
    assert_eq!(
        hub_asset_url("http://hub", "https://objects.example/a"),
        "https://objects.example/a"
    );
}

#[test]
fn approval_url_keeps_code_in_fragment() {
    let tags = vec!["gpu,fast".to_string(), "region:us west".to_string()];
    let code = "0123456789abcdef0123456789abcdef01234567";
    assert_eq!(
        approval_url("https://app/", code, "lab rover", 2, &tags),
        "https://app/rovers#enroll=0123456789abcdef0123456789abcdef01234567&name=lab%20rover&units=2&tag=gpu%2Cfast&tag=region%3Aus%20west"
    );
}

#[test]
fn discovery_web_url_is_required_for_browser_enrollment() {
    assert_eq!(
        discovery_web_url(&json!({ "web_url": "http://localhost:3000/" })).as_deref(),
        Some("http://localhost:3000")
    );
    assert!(discovery_web_url(&json!({ "web_url": "" })).is_none());
    assert!(discovery_web_url(&json!({})).is_none());
}

#[test]
fn approval_url_has_native_opener_on_supported_platforms() {
    #[cfg(any(unix, windows))]
    assert!(approval_open_command("https://example.com").is_some());
}

#[test]
fn rover_units_are_capped() {
    assert_eq!(validate_rover_units(100).unwrap(), 100);
    assert!(validate_rover_units(101).is_err());
    assert_eq!(clamp_rover_units(101), 100);
}

#[test]
fn upgrade_waits_for_active_runs() {
    tokio::runtime::Runtime::new().unwrap().block_on(async {
        let active = Arc::new(AtomicUsize::new(1));
        let done = Arc::new(AtomicBool::new(false));
        let waiter_active = active.clone();
        let waiter_done = done.clone();
        tokio::spawn(async move {
            wait_for_active_runs(&waiter_active, Duration::from_millis(10)).await;
            waiter_done.store(true, Ordering::Relaxed);
        });

        sleep(Duration::from_millis(30)).await;
        assert!(!done.load(Ordering::Relaxed));
        active.store(0, Ordering::Relaxed);
        sleep(Duration::from_millis(30)).await;
        assert!(done.load(Ordering::Relaxed));
    });
}

#[test]
fn safe_asset_filename_removes_path_segments() {
    assert_eq!(safe_asset_filename("../trace log.txt"), "trace_log.txt");
    assert_eq!(safe_asset_filename(".."), "asset");
    assert_eq!(safe_asset_filename("a/b\\c.bin"), "c.bin");
}

#[test]
fn local_asset_paths_are_scoped_and_replaceable() {
    let root = std::env::temp_dir().join(format!(
        "ufo-test-op-{}-{}",
        std::process::id(),
        chrono::Utc::now().timestamp_nanos_opt().unwrap_or_default()
    ));
    let op = root.join("run");
    fs::create_dir_all(&op).unwrap();
    let inside = op.join("report.pdf");
    fs::write(&inside, "report").unwrap();
    fs::create_dir_all(op.join(".git")).unwrap();
    fs::write(op.join(".git").join("config"), "secret").unwrap();
    fs::write(op.join(".ufo-work-directory"), "marker").unwrap();
    let traversal = op.join("..").join("outside.pdf");
    let outside = std::env::temp_dir().join("report.pdf");
    fs::write(root.join("outside.pdf"), "outside").unwrap();
    let msg = format!(
        "see file://{} and {} plus {} and {} and {}",
        inside.display(),
        traversal.display(),
        outside.display(),
        op.join(".git").join("config").display(),
        op.join(".ufo-work-directory").display()
    );

    let refs = referenced_local_paths(&msg, &op);
    assert_eq!(refs.len(), 1);
    assert_eq!(refs[0].raw, inside);
    assert_eq!(
        replace_path_refs(&msg, &refs[0].raw, "/v1/assets/a/file"),
        format!(
            "see /v1/assets/a/file and {} plus {} and {} and {}",
            traversal.display(),
            outside.display(),
            op.join(".git").join("config").display(),
            op.join(".ufo-work-directory").display()
        )
    );
    let md_msg = format!("generated [preview]({})", inside.display());
    let refs = referenced_local_paths(&md_msg, &op);
    assert_eq!(refs.len(), 1);
    assert_eq!(
        replace_path_refs(&md_msg, &refs[0].raw, "/v1/assets/a/file"),
        "generated [preview](/v1/assets/a/file)"
    );
    let _ = fs::remove_dir_all(root);
}

fn test_outpost() -> PathBuf {
    std::env::temp_dir().join("ufo-test-outpost")
}

fn sample_entry(name: &str) -> RoverEntry {
    RoverEntry {
        hub: "http://localhost:8080".to_string(),
        token: format!("{name}-token"),
        fleet_id: Some("fleet-id".to_string()),
        fleet_name: Some("Fleet".to_string()),
        name: name.to_string(),
        units: 2,
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
fn sse_frame_parses_event_and_ignores_keepalive() {
    let frame =
        ": keepalive\nevent: config\ndata: {\"name\":\"lab\",\"units\":3,\"tags\":[\"gpu\"]}\n\n";
    let (event, data) = parse_sse_frame(frame).expect("config frame");
    assert_eq!(event, "config");
    assert_eq!(data, "{\"name\":\"lab\",\"units\":3,\"tags\":[\"gpu\"]}");
    let config: RoverConfigEvent = serde_json::from_str(&data).unwrap();
    assert_eq!(config.name.as_deref(), Some("lab"));
    assert_eq!(config.units, Some(3));
    assert_eq!(config.tags, Some(vec!["gpu".to_string()]));
    assert!(parse_sse_frame(": just a comment\n\n").is_none());
}

#[test]
fn enroll_config_splits_key_values_and_tags() {
    let fields =
        parse_enroll_config("hub=http://h:8080|code=abc|name=alpha|units=5|tags=gpu,region:us");
    assert_eq!(fields.get("code").map(String::as_str), Some("abc"));
    assert_eq!(fields.get("hub").map(String::as_str), Some("http://h:8080"));
    assert_eq!(fields.get("name").map(String::as_str), Some("alpha"));
    assert_eq!(fields.get("units").map(String::as_str), Some("5"));
    assert_eq!(
        fields.get("tags").map(String::as_str),
        Some("gpu,region:us")
    );
    assert_eq!(
        parse_tags_list(fields.get("tags").unwrap()),
        vec!["gpu".to_string(), "region:us".to_string()]
    );
}

#[test]
fn enroll_config_unescapes_pipe_in_values() {
    let fields = parse_enroll_config(r"code=ab\|c|name=a\|b|tags=gpu,region:moon");
    assert_eq!(fields.get("code").map(String::as_str), Some("ab|c"));
    assert_eq!(fields.get("name").map(String::as_str), Some("a|b"));
    assert_eq!(
        parse_tags_list(fields.get("tags").unwrap()),
        vec!["gpu".to_string(), "region:moon".to_string()]
    );
}

#[test]
fn parse_tags_list_keeps_colons_and_drops_empty() {
    assert_eq!(
        parse_tags_list("gpu, region:moon, ,os:macos"),
        vec![
            "gpu".to_string(),
            "region:moon".to_string(),
            "os:macos".to_string()
        ]
    );
    assert!(parse_tags_list("").is_empty());
    assert!(parse_tags_list("  , , ").is_empty());
}

#[test]
fn field_or_env_prefers_field_then_env() {
    let _guard = env_lock();
    let key = "UFO_TEST_FIELD_OR_ENV";
    // SAFETY: held under ENV_LOCK for this module's env-mutating tests.
    unsafe {
        std::env::remove_var(key);
    }
    assert_eq!(field_or_env(None, key), None);
    assert_eq!(
        field_or_env(Some(&"from-field".to_string()), key),
        Some("from-field".to_string())
    );
    unsafe {
        std::env::set_var(key, "from-env");
    }
    assert_eq!(field_or_env(None, key), Some("from-env".to_string()));
    assert_eq!(
        field_or_env(Some(&"from-field".to_string()), key),
        Some("from-field".to_string())
    );
    assert_eq!(
        field_or_env(Some(&String::new()), key),
        Some("from-env".to_string())
    );
    unsafe {
        std::env::remove_var(key);
    }
}

#[test]
fn hub_url_appends_version() {
    assert_eq!(
        hub_url("http://h:8080", "runs/accept"),
        "http://h:8080/v1/runs/accept"
    );
    assert_eq!(
        hub_url("http://h:8080/", "/rovers/abc"),
        "http://h:8080/v1/rovers/abc"
    );
    assert_eq!(hub_health_url("http://h:8080/"), "http://h:8080/healthz");
}

#[test]
fn hub_host_uses_origin_host() {
    assert_eq!(hub_host("https://hub.example.com/base/"), "hub.example.com");
    assert_eq!(hub_host("localhost:8080/path"), "localhost:8080");
    assert_eq!(fleet_label(&sample_entry("alpha")), "Fleet");
}

#[test]
fn sub_operations_sentinel_is_parsed_and_stripped() {
    let msg = "Planned it.\n@@UFO_SUB_OPERATIONS@@\n[{\"title\":\"A\"},{\"title\":\"B\"}]";
    let sub_operations = parse_sub_operations(msg).expect("sub-operations parse");
    assert_eq!(sub_operations.as_array().unwrap().len(), 2);
    assert_eq!(strip_sub_operations(msg), "Planned it.");
    assert!(parse_sub_operations("just a reply").is_none());
    assert!(parse_sub_operations("x\n@@UFO_SUB_OPERATIONS@@\nnot json").is_none());
}

#[test]
fn operations_sentinel_is_parsed_and_stripped() {
    let msg = "Created a follow-up.\n@@UFO_OPERATIONS@@\n[{\"title\":\"Follow up\",\"body\":\"Discuss memory\"}]";
    let operations = parse_operations(msg).expect("operations parse");
    assert_eq!(operations.as_array().unwrap().len(), 1);
    assert_eq!(strip_operations(msg), "Created a follow-up.");
    assert!(parse_operations("just a reply").is_none());
    assert!(parse_operations("x\n@@UFO_OPERATIONS@@\nnot json").is_none());
}

#[test]
fn sub_operations_feedback_sentinel_is_parsed_and_stripped() {
    let msg = "Needs redo.\n@@UFO_SUB_OPERATIONS_FEEDBACK@@\n[{\"operation_id\":\"op\",\"body\":\"fix it\"}]";
    let feedback = parse_sub_operations_feedback(msg).expect("feedback parse");
    assert_eq!(feedback.as_array().unwrap().len(), 1);
    assert_eq!(strip_sub_operations_feedback(msg), "Needs redo.");
    assert!(parse_sub_operations_feedback("just a reply").is_none());
    assert!(
        parse_sub_operations_feedback("x\n@@UFO_SUB_OPERATIONS_FEEDBACK@@\nnot json").is_none()
    );
}

#[test]
fn mission_learning_sentinel_is_parsed_and_stripped() {
    let msg = "Finished.\n@@UFO_MISSION_LEARNING@@ {\"summary\":\"Reuse the parser.\",\"artifacts\":[{\"kind\":\"doc\",\"path\":\"docs/parser.md\"}]}\n@@UFO_STATUS:done@@";
    let learning = parse_mission_learning(msg).expect("mission learning parse");
    assert_eq!(learning["summary"], "Reuse the parser.");
    assert_eq!(
        strip_mission_learning(msg),
        "Finished.\n@@UFO_STATUS:done@@"
    );
    assert!(parse_mission_learning("x\n@@UFO_MISSION_LEARNING@@ not json").is_none());
}

#[test]
fn co_occurring_directives_parse_and_strip_independently() {
    let msg = "Done.\n@@UFO_OPERATIONS@@ [{\"title\":\"A\",\"body\":\"b\"}]\n@@UFO_MISSION_LEARNING@@ {\"summary\":\"Reuse it.\"}";
    assert_eq!(parse_operations(msg).unwrap().as_array().unwrap().len(), 1);
    assert_eq!(
        parse_mission_learning(msg).expect("learning")["summary"],
        "Reuse it."
    );
    let stripped = strip_mission_learning(&strip_operations(msg));
    assert_eq!(stripped, "Done.");
    let reversed = "Done.\n@@UFO_MISSION_LEARNING@@ {\"summary\":\"Reuse it.\"}\n@@UFO_SUB_OPERATIONS@@ [{\"title\":\"A\",\"body\":\"b\"}]";
    assert_eq!(
        parse_sub_operations(reversed)
            .unwrap()
            .as_array()
            .unwrap()
            .len(),
        1
    );
    assert_eq!(
        parse_mission_learning(reversed).expect("learning")["summary"],
        "Reuse it."
    );
}

#[test]
fn status_sentinel_without_closer_is_parsed_and_stripped() {
    let msg = "Almost there.\n@@UFO_STATUS:done";

    assert_eq!(parse_status(msg).as_deref(), Some("done"));
    assert_eq!(strip_status(msg), "Almost there.");
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
    cfg.rovers.insert("def222".to_string(), sample_entry("two"));
    cfg.rovers
        .insert("ghi333".to_string(), sample_entry("three"));

    assert_eq!(resolve_rover_id(&cfg, "ghi").unwrap(), "ghi333");
    assert!(resolve_rover_id(&cfg, "").is_err());
    assert!(resolve_rover_id(&cfg, "zzz").is_err());
}

#[test]
fn operation_directory_rejects_path_traversal() {
    let base = std::env::temp_dir().join("ufo-operations");

    assert_eq!(
        operation_directory_path(
            &base,
            "18181818-0606-3333-8888-952795279527",
            "UFO-20-ui",
            "2026-06-18T18:18:18Z"
        )
        .unwrap(),
        base.join("2026")
            .join("06")
            .join("18")
            .join("18")
            .join("UFO-20-ui")
    );
    assert!(operation_directory_path(&base, "../escape", "", "2026-06-18T18:18:18Z").is_err());
    assert!(
        operation_directory_path(&base, "nested/operation", "", "2026-06-18T18:18:18Z").is_err()
    );
    assert!(
        operation_directory_path(&base, "operation", "../escape", "2026-06-18T18:18:18Z").is_err()
    );
    assert!(operation_directory_path(&base, "", "", "2026-06-18T18:18:18Z").is_err());
    assert!(operation_directory_path(&base, "operation", "", "bad-date").is_err());
    assert_eq!(
        operation_directory_path(
            &base,
            "operation",
            "UFO-20-修-worktree",
            "2026-06-18T18:18:18Z"
        )
        .unwrap(),
        base.join("2026")
            .join("06")
            .join("18")
            .join("op")
            .join("UFO-20-修-worktree")
    );
}

#[test]
fn worktree_start_point_falls_back_when_preferred_missing() {
    let _guard = env_lock();
    tokio::runtime::Runtime::new().unwrap().block_on(async {
        let base = std::env::temp_dir().join(format!(
            "ufo-worktree-fallback-{}",
            chrono::Local::now()
                .timestamp_nanos_opt()
                .unwrap_or_default()
        ));
        let source = base.join("source");
        fs::create_dir_all(&source).unwrap();
        init_test_git_repo(&source).await.unwrap();
        fs::write(source.join("README.md"), "main tip\n").unwrap();
        git(&source, &["add", "."]).await.unwrap();
        git(&source, &["commit", "-q", "-m", "main"]).await.unwrap();
        git(&source, &["branch", "orbit"]).await.unwrap();
        fs::write(source.join("README.md"), "orbit tip\n").unwrap();
        git(&source, &["add", "."]).await.unwrap();
        git(&source, &["commit", "-q", "-m", "on-orbit"])
            .await
            .unwrap();
        git(&source, &["checkout", "-q", "main"]).await.unwrap();

        let start = resolve_worktree_start_point(&source, "feature/auto", "orbit").await;
        assert_eq!(
            start, "orbit",
            "missing preferred tip should use ship base fallback"
        );
        let start = resolve_worktree_start_point(&source, "feature/auto", "").await;
        assert_eq!(
            start, "HEAD",
            "missing preferred and fallback should use HEAD"
        );
        let _ = fs::remove_dir_all(base);
    });
}

#[test]
fn work_directory_uses_source_worktree_for_git_diff() {
    let _guard = env_lock();
    tokio::runtime::Runtime::new().unwrap().block_on(async {
        let base = std::env::temp_dir().join(format!(
            "ufo-worktree-test-{}",
            chrono::Local::now()
                .timestamp_nanos_opt()
                .unwrap_or_default()
        ));
        let source = base.join("source");
        let operation = base.join("operation");
        fs::create_dir_all(&source).unwrap();
        init_test_git_repo(&source).await.unwrap();
        fs::write(source.join("README.md"), "before\n").unwrap();
        fs::write(source.join(".gitignore"), "ignored-secret.txt\n").unwrap();
        git(&source, &["add", "."]).await.unwrap();
        git(&source, &["commit", "-q", "-m", "base"]).await.unwrap();
        fs::write(source.join("README.md"), "dirty source\n").unwrap();
        fs::write(source.join("scratch.txt"), "untracked\n").unwrap();
        fs::write(source.join("ignored-secret.txt"), "secret\n").unwrap();

        ensure_work_directory(&operation, Some(&source), "", "")
            .await
            .unwrap();
        assert_eq!(
            fs::read_to_string(operation.join("README.md")).unwrap(),
            "dirty source\n"
        );
        assert_eq!(
            fs::read_to_string(operation.join("scratch.txt")).unwrap(),
            "untracked\n"
        );
        assert!(!operation.join("ignored-secret.txt").exists());
        fs::create_dir_all(operation.join("assets")).unwrap();
        fs::write(operation.join("assets").join("note.txt"), "asset\n").unwrap();
        fs::write(operation.join("README.md"), "after\n").unwrap();
        let cache = operation_asset_cache_dir(&operation).await.unwrap();
        assert!(!cache.starts_with(&operation));
        fs::create_dir_all(&cache).unwrap();
        fs::write(cache.join("input.txt"), "asset\n").unwrap();
        let status = git(&operation, &["status", "--short", "--untracked-files=all"])
            .await
            .unwrap();
        assert!(!status.contains("input.txt"));
        git(&operation, &["add", "-N", "."]).await.unwrap();
        let diff = git_diff(&operation).await.unwrap();
        assert!(diff.contains("-before"));
        assert!(diff.contains("+after"));
        assert!(!diff.contains("assets/note.txt"));
        let source_exclude = fs::read_to_string(source.join(".git").join("info").join("exclude"))
            .unwrap_or_default();
        assert!(!source_exclude.lines().any(|line| line.trim() == "/assets/"));

        let _ = fs::remove_dir_all(base);
    });
}

#[test]
fn source_apply_to_source_applies_only_when_touched_paths_are_clean() {
    let _guard = env_lock();
    tokio::runtime::Runtime::new().unwrap().block_on(async {
        let base = std::env::temp_dir().join(format!(
            "ufo-source-apply-test-{}",
            chrono::Local::now()
                .timestamp_nanos_opt()
                .unwrap_or_default()
        ));
        let source = base.join("source");
        let operation = base.join("operation");
        fs::create_dir_all(&source).unwrap();
        init_test_git_repo(&source).await.unwrap();
        fs::write(source.join("README.md"), "before\n").unwrap();
        fs::write(source.join("notes.txt"), "clean\n").unwrap();
        git(&source, &["add", "."]).await.unwrap();
        git(&source, &["commit", "-q", "-m", "base"]).await.unwrap();

        ensure_work_directory(&operation, Some(&source), "", "")
            .await
            .unwrap();
        fs::write(operation.join("README.md"), "applied\n").unwrap();
        fs::write(operation.join("TODO.md"), "new\n").unwrap();
        fs::create_dir_all(operation.join("assets")).unwrap();
        fs::write(operation.join("assets").join("note.txt"), "asset\n").unwrap();
        fs::write(source.join("notes.txt"), "unrelated dirty\n").unwrap();

        let report = apply_operation_to_source(&source, &operation)
            .await
            .unwrap();

        assert_eq!(report.status, "succeeded");
        assert_eq!(
            fs::read_to_string(source.join("README.md")).unwrap(),
            "applied\n"
        );
        assert_eq!(
            fs::read_to_string(source.join("notes.txt")).unwrap(),
            "unrelated dirty\n"
        );
        assert_eq!(fs::read_to_string(source.join("TODO.md")).unwrap(), "new\n");
        assert!(!source.join("assets").join("note.txt").exists());
        let status = git(
            &operation,
            &["status", "--porcelain=v1", "--untracked-files=all"],
        )
        .await
        .unwrap();
        assert!(status.contains("?? TODO.md"));
        assert!(!status.contains(" A TODO.md"));

        let _ = fs::remove_dir_all(base);
    });
}

#[test]
fn source_report_metadata_includes_source_repo_address() {
    let _guard = env_lock();
    tokio::runtime::Runtime::new().unwrap().block_on(async {
        let base = std::env::temp_dir().join(format!(
            "ufo-source-meta-test-{}",
            chrono::Local::now()
                .timestamp_nanos_opt()
                .unwrap_or_default()
        ));
        let source = base.join("source");
        fs::create_dir_all(&source).unwrap();
        init_test_git_repo(&source).await.unwrap();
        git(
            &source,
            &["remote", "add", "origin", "git@example.com:ufo/source.git"],
        )
        .await
        .unwrap();

        let metadata = source_report_metadata(
            &source,
            Some(serde_json::json!({ "blocking_paths": ["README.md"] })),
        )
        .await
        .unwrap();

        assert_eq!(metadata["source_path"], source.display().to_string());
        assert_eq!(
            metadata["source_remote_url"],
            "git@example.com:ufo/source.git"
        );
        assert_eq!(metadata["blocking_paths"][0], "README.md");

        let _ = fs::remove_dir_all(base);
    });
}

#[test]
fn source_refresh_updates_from_source_head_without_conflict() {
    let _guard = env_lock();
    tokio::runtime::Runtime::new().unwrap().block_on(async {
        let base = std::env::temp_dir().join(format!(
            "ufo-source-refresh-test-{}",
            chrono::Local::now()
                .timestamp_nanos_opt()
                .unwrap_or_default()
        ));
        let source = base.join("source");
        let operation = base.join("operation");
        fs::create_dir_all(&source).unwrap();
        init_test_git_repo(&source).await.unwrap();
        fs::write(source.join("README.md"), "before\n").unwrap();
        git(&source, &["add", "."]).await.unwrap();
        git(&source, &["commit", "-q", "-m", "base"]).await.unwrap();

        ensure_work_directory(&operation, Some(&source), "", "")
            .await
            .unwrap();
        fs::write(operation.join("README.md"), "operation edit\n").unwrap();
        fs::write(operation.join("TODO.md"), "new operation file\n").unwrap();
        fs::write(source.join("notes.txt"), "source update\n").unwrap();
        git(&source, &["add", "."]).await.unwrap();
        git(&source, &["commit", "-q", "-m", "source update"])
            .await
            .unwrap();
        let source_head = git_head(&source).await.unwrap();

        let report = refresh_operation_from_source(&source, &operation)
            .await
            .unwrap();

        assert_eq!(report.status, "succeeded");
        assert_eq!(report.source_head_sha, source_head);
        assert_eq!(git_head(&operation).await.unwrap(), source_head);
        assert_eq!(
            fs::read_to_string(operation.join("README.md")).unwrap(),
            "operation edit\n"
        );
        assert_eq!(
            fs::read_to_string(operation.join("notes.txt")).unwrap(),
            "source update\n"
        );
        assert_eq!(
            fs::read_to_string(operation.join("TODO.md")).unwrap(),
            "new operation file\n"
        );

        let _ = fs::remove_dir_all(base);
    });
}

#[test]
fn source_refresh_conflict_keeps_operation_worktree_unchanged() {
    let _guard = env_lock();
    tokio::runtime::Runtime::new().unwrap().block_on(async {
        let base = std::env::temp_dir().join(format!(
            "ufo-source-refresh-conflict-test-{}",
            chrono::Local::now()
                .timestamp_nanos_opt()
                .unwrap_or_default()
        ));
        let source = base.join("source");
        let operation = base.join("operation");
        fs::create_dir_all(&source).unwrap();
        init_test_git_repo(&source).await.unwrap();
        fs::write(source.join("README.md"), "before\n").unwrap();
        git(&source, &["add", "."]).await.unwrap();
        git(&source, &["commit", "-q", "-m", "base"]).await.unwrap();

        ensure_work_directory(&operation, Some(&source), "", "")
            .await
            .unwrap();
        let original_head = git_head(&operation).await.unwrap();
        fs::write(operation.join("README.md"), "operation edit\n").unwrap();
        fs::write(source.join("README.md"), "source edit\n").unwrap();
        git(&source, &["add", "."]).await.unwrap();
        git(&source, &["commit", "-q", "-m", "source edit"])
            .await
            .unwrap();

        let report = refresh_operation_from_source(&source, &operation)
            .await
            .unwrap();

        assert_eq!(report.status, "conflicted");
        assert_eq!(git_head(&operation).await.unwrap(), original_head);
        assert_eq!(
            fs::read_to_string(operation.join("README.md")).unwrap(),
            "operation edit\n"
        );

        let _ = fs::remove_dir_all(base);
    });
}

#[test]
fn conflict_marker_scan_detects_real_git_markers_not_mentions() {
    let (open, close) = git_conflict_marker_pair();
    let conflicted = format!("{open} HEAD\nmine\n=======\ntheirs\n{close} branch\n");
    assert!(text_has_git_conflict_markers(&conflicted));

    let mention = format!("if text.contains(\"{open}\") && text.contains(\"{close}\") {{\n");
    assert!(!text_has_git_conflict_markers(&mention));
    assert!(!text_has_git_conflict_markers("no markers here\n"));

    let main_rs = fs::read_to_string(Path::new(env!("CARGO_MANIFEST_DIR")).join("src/main.rs"))
        .expect("read main.rs");
    assert!(
        !main_rs.contains(&open) || !main_rs.contains(&close),
        "main.rs must not embed both full git conflict marker sequences as literals"
    );
}

#[test]
fn drops_worktree_only_for_auto_loop_branch_success() {
    assert!(drop_operation_worktree_after_branch_action(
        "commit_to_branch",
        "succeeded",
        true
    ));
    assert!(drop_operation_worktree_after_branch_action(
        "create_source_branch",
        "succeeded",
        true
    ));
    assert!(!drop_operation_worktree_after_branch_action(
        "create_source_branch",
        "succeeded",
        false
    ));
    assert!(!drop_operation_worktree_after_branch_action(
        "commit_to_branch",
        "succeeded",
        false
    ));
    assert!(!drop_operation_worktree_after_branch_action(
        "commit_to_branch",
        "failed",
        true
    ));
    assert!(!drop_operation_worktree_after_branch_action(
        "apply_to_source",
        "succeeded",
        true
    ));
}

#[test]
fn auto_loop_branch_success_removes_operation_worktree() {
    let _guard = env_lock();
    tokio::runtime::Runtime::new().unwrap().block_on(async {
        let base = std::env::temp_dir().join(format!(
            "ufo-drop-worktree-test-{}",
            chrono::Local::now()
                .timestamp_nanos_opt()
                .unwrap_or_default()
        ));
        let source = base.join("source");
        let operation = base.join("operation");
        fs::create_dir_all(&source).unwrap();
        init_test_git_repo(&source).await.unwrap();
        fs::write(source.join("README.md"), "before\n").unwrap();
        git(&source, &["add", "."]).await.unwrap();
        git(&source, &["commit", "-q", "-m", "base"]).await.unwrap();

        ensure_work_directory(&operation, Some(&source), "", "")
            .await
            .unwrap();
        fs::write(operation.join("README.md"), "on-branch\n").unwrap();
        let action = AcceptedSourceAction {
            id: "action".to_string(),
            operation_id: "operation".to_string(),
            operation_title: "Branch work".to_string(),
            operation_worktree_name: "UFO-9-drop".to_string(),
            operation_created_at: "2026-06-18T18:18:18Z".to_string(),
            kind: "commit_to_branch".to_string(),
            branch_name: "feature/skill-bindings".to_string(),
            drop_worktree_on_success: true,
            checks_commands: vec![],
            checks_timeout_seconds: 0,
        };

        let report = branch_operation_changes(&source, &operation, &action, true)
            .await
            .unwrap();
        assert_eq!(report.status, "succeeded");

        if drop_operation_worktree_after_branch_action(
            &action.kind,
            &report.status,
            action.drop_worktree_on_success,
        ) {
            remove_worktree(&source, &operation).await;
        }
        assert!(
            !operation.exists() || !operation.join(".git").exists(),
            "auto-loop worktree should be removed after branch lands"
        );
        assert_eq!(
            git(&source, &["show", "feature/skill-bindings:README.md"])
                .await
                .unwrap(),
            "on-branch\n"
        );
        let _ = fs::remove_dir_all(base);
    });
}

#[test]
fn commit_to_branch_fails_pre_commit_checks_without_committing() {
    let _guard = env_lock();
    tokio::runtime::Runtime::new().unwrap().block_on(async {
        let base = std::env::temp_dir().join(format!(
            "ufo-pre-commit-check-{}",
            chrono::Local::now()
                .timestamp_nanos_opt()
                .unwrap_or_default()
        ));
        let source = base.join("source");
        let operation = base.join("operation");
        fs::create_dir_all(&source).unwrap();
        init_test_git_repo(&source).await.unwrap();
        fs::write(source.join("README.md"), "before\n").unwrap();
        git(&source, &["add", "."]).await.unwrap();
        git(&source, &["commit", "-q", "-m", "base"]).await.unwrap();

        ensure_work_directory(&operation, Some(&source), "", "")
            .await
            .unwrap();
        fs::write(operation.join("README.md"), "broken\n").unwrap();
        let action = AcceptedSourceAction {
            id: "action".to_string(),
            operation_id: "operation".to_string(),
            operation_title: "Fail checks".to_string(),
            operation_worktree_name: "UFO-checks".to_string(),
            operation_created_at: "2026-06-18T18:18:18Z".to_string(),
            kind: "commit_to_branch".to_string(),
            branch_name: "feature/auto".to_string(),
            drop_worktree_on_success: true,
            checks_commands: vec![
                "true".to_string(),
                "echo check-fail-stderr >&2; exit 17".to_string(),
            ],
            checks_timeout_seconds: 30,
        };

        let report = branch_operation_changes(&source, &operation, &action, true)
            .await
            .unwrap();
        assert_eq!(report.status, "failed");
        assert!(
            report
                .message
                .contains("pre-commit checks[1] failed (exit 17)"),
            "message should include index and exit: {}",
            report.message
        );
        assert!(
            report.message.contains("check-fail-stderr"),
            "message should include stderr tail: {}",
            report.message
        );
        assert!(
            !git_success(
                &source,
                &["show-ref", "--verify", "--quiet", "refs/heads/feature/auto"]
            )
            .await
            .unwrap(),
            "failed checks must not create the branch"
        );
        assert!(
            operation.join(".git").exists(),
            "worktree must remain after check failure"
        );
        assert!(!drop_operation_worktree_after_branch_action(
            &action.kind,
            &report.status,
            action.drop_worktree_on_success,
        ));

        let _ = fs::remove_dir_all(base);
    });
}

#[test]
fn windows_wsl_bash_stub_paths_are_detected() {
    assert!(is_windows_wsl_bash_stub(Path::new(
        r"C:\Windows\System32\bash.exe"
    )));
    assert!(is_windows_wsl_bash_stub(Path::new(
        r"C:\Windows\System32\wsl.exe"
    )));
    assert!(is_windows_wsl_bash_stub(Path::new(
        r"C:\Program Files\WindowsApps\MicrosoftCorporationII.WindowsSubsystemForLinux_1.0\bash.exe"
    )));
    assert!(!is_windows_wsl_bash_stub(Path::new(
        r"C:\Program Files\Git\bin\bash.exe"
    )));
    assert!(!is_windows_wsl_bash_stub(Path::new("/usr/bin/bash")));
}

#[test]
fn run_bash_check_commands_empty_is_noop() {
    let _guard = env_lock();
    tokio::runtime::Runtime::new().unwrap().block_on(async {
        let dir = std::env::temp_dir();
        assert!(
            run_bash_check_commands(&dir, &[], 5, "pre-commit")
                .await
                .is_none()
        );
        assert!(
            run_bash_check_commands(&dir, &[String::new(), "   ".to_string()], 5, "pre-commit")
                .await
                .is_none()
        );
    });
}

#[test]
fn source_branch_commits_operation_changes_without_switching_source() {
    let _guard = env_lock();
    tokio::runtime::Runtime::new().unwrap().block_on(async {
        let base = std::env::temp_dir().join(format!(
            "ufo-source-branch-test-{}",
            chrono::Local::now()
                .timestamp_nanos_opt()
                .unwrap_or_default()
        ));
        let source = base.join("source");
        let operation = base.join("operation");
        fs::create_dir_all(&source).unwrap();
        init_test_git_repo(&source).await.unwrap();
        fs::write(source.join("README.md"), "before\n").unwrap();
        git(&source, &["add", "."]).await.unwrap();
        git(&source, &["commit", "-q", "-m", "base"]).await.unwrap();

        ensure_work_directory(&operation, Some(&source), "", "")
            .await
            .unwrap();
        fs::write(operation.join("README.md"), "branched\n").unwrap();
        let action = AcceptedSourceAction {
            id: "action".to_string(),
            operation_id: "operation".to_string(),
            operation_title: "Branch work".to_string(),
            operation_worktree_name: "UFO-2-branch-work".to_string(),
            operation_created_at: "2026-06-18T18:18:18Z".to_string(),
            kind: "create_source_branch".to_string(),
            branch_name: "ufo/UFO-2-branch-work".to_string(),
            drop_worktree_on_success: false,
            checks_commands: vec![],
            checks_timeout_seconds: 0,
        };

        let report = branch_operation_changes(&source, &operation, &action, false)
            .await
            .unwrap();

        assert_eq!(report.status, "succeeded");
        assert_eq!(report.branch_name, "ufo/UFO-2-branch-work");
        assert!(!report.commit_sha.is_empty());
        let first_commit = report.commit_sha.clone();
        assert_eq!(
            fs::read_to_string(source.join("README.md")).unwrap(),
            "before\n"
        );
        assert_eq!(
            git(&source, &["show", "ufo/UFO-2-branch-work:README.md"])
                .await
                .unwrap(),
            "branched\n"
        );
        git(&source, &["branch", "-D", "ufo/UFO-2-branch-work"])
            .await
            .unwrap();

        let recreated = branch_operation_changes(&source, &operation, &action, false)
            .await
            .unwrap();

        assert_eq!(recreated.status, "succeeded");
        assert_eq!(recreated.commit_sha, first_commit);
        assert_eq!(
            git(&source, &["show", "ufo/UFO-2-branch-work:README.md"])
                .await
                .unwrap(),
            "branched\n"
        );
        git(&source, &["branch", "-D", "ufo/UFO-2-branch-work"])
            .await
            .unwrap();
        fs::write(operation.join("README.md"), "branched again\n").unwrap();

        let updated = branch_operation_changes(&source, &operation, &action, false)
            .await
            .unwrap();

        assert_eq!(updated.status, "succeeded");
        assert_ne!(updated.commit_sha, first_commit);
        assert_eq!(
            git(&source, &["show", "ufo/UFO-2-branch-work:README.md"])
                .await
                .unwrap(),
            "branched again\n"
        );

        fs::write(operation.join("README.md"), "loop iteration\n").unwrap();
        let iterate = branch_operation_changes(&source, &operation, &action, true)
            .await
            .unwrap();
        assert_eq!(iterate.status, "succeeded");
        assert_ne!(iterate.commit_sha, updated.commit_sha);
        assert_eq!(
            git(&source, &["show", "ufo/UFO-2-branch-work:README.md"])
                .await
                .unwrap(),
            "loop iteration\n"
        );

        let _ = fs::remove_dir_all(base);
    });
}

#[test]
fn marker_work_directory_migrates_to_source_worktree_and_keeps_assets() {
    let _guard = env_lock();
    tokio::runtime::Runtime::new().unwrap().block_on(async {
        let base = std::env::temp_dir().join(format!(
            "ufo-marker-worktree-test-{}",
            chrono::Local::now()
                .timestamp_nanos_opt()
                .unwrap_or_default()
        ));
        let source = base.join("source");
        let operation = base.join("operation");
        fs::create_dir_all(&source).unwrap();
        init_test_git_repo(&source).await.unwrap();
        fs::write(source.join("README.md"), "source\n").unwrap();
        git(&source, &["add", "."]).await.unwrap();
        git(&source, &["commit", "-q", "-m", "base"]).await.unwrap();

        ensure_work_directory(&operation, None, "", "")
            .await
            .unwrap();
        fs::create_dir_all(operation.join("assets")).unwrap();
        fs::write(operation.join("assets").join("note.txt"), "asset\n").unwrap();
        ensure_work_directory(&operation, Some(&source), "", "")
            .await
            .unwrap();

        assert_eq!(
            fs::read_to_string(operation.join("README.md")).unwrap(),
            "source\n"
        );
        assert_eq!(
            fs::read_to_string(operation.join("assets").join("note.txt")).unwrap(),
            "asset\n"
        );
        assert!(!operation.join(".ufo-work-directory").exists());

        let _ = fs::remove_dir_all(base);
    });
}

#[test]
fn work_directory_fails_when_source_worktree_cannot_be_created() {
    let _guard = env_lock();
    tokio::runtime::Runtime::new().unwrap().block_on(async {
        let base = std::env::temp_dir().join(format!(
            "ufo-worktree-fail-test-{}",
            chrono::Local::now()
                .timestamp_nanos_opt()
                .unwrap_or_default()
        ));
        let source = base.join("source");
        let operation = base.join("operation");
        fs::create_dir_all(&source).unwrap();
        init_test_git_repo(&source).await.unwrap();
        fs::write(source.join("README.md"), "source\n").unwrap();
        git(&source, &["add", "."]).await.unwrap();
        git(&source, &["commit", "-q", "-m", "base"]).await.unwrap();
        fs::create_dir_all(&operation).unwrap();
        fs::write(operation.join("stray.txt"), "not a worktree\n").unwrap();

        let err = ensure_work_directory(&operation, Some(&source), "", "")
            .await
            .unwrap_err()
            .to_string();
        assert!(err.contains("operation directory is not empty"));
        assert!(!operation.join(".git").exists());

        let file_operation = base.join("operation-file");
        fs::write(&file_operation, "not a directory\n").unwrap();
        let err = ensure_work_directory(&file_operation, Some(&source), "", "")
            .await
            .unwrap_err()
            .to_string();
        assert!(err.contains("operation directory is not a directory"));
        assert_eq!(
            fs::read_to_string(&file_operation).unwrap(),
            "not a directory\n"
        );

        let _ = fs::remove_dir_all(base);
    });
}

#[test]
fn work_directory_rejects_unrelated_existing_git_repo() {
    let _guard = env_lock();
    tokio::runtime::Runtime::new().unwrap().block_on(async {
        let base = std::env::temp_dir().join(format!(
            "ufo-unrelated-worktree-test-{}",
            chrono::Local::now()
                .timestamp_nanos_opt()
                .unwrap_or_default()
        ));
        let source = base.join("source");
        let operation = base.join("operation");
        fs::create_dir_all(&source).unwrap();
        init_test_git_repo(&source).await.unwrap();
        fs::write(source.join("README.md"), "source\n").unwrap();
        git(&source, &["add", "."]).await.unwrap();
        git(&source, &["commit", "-q", "-m", "base"]).await.unwrap();

        fs::create_dir_all(&operation).unwrap();
        init_test_git_repo(&operation).await.unwrap();
        fs::write(operation.join("README.md"), "unrelated\n").unwrap();
        git(&operation, &["add", "."]).await.unwrap();
        git(&operation, &["commit", "-q", "-m", "base"])
            .await
            .unwrap();

        let err = ensure_work_directory(&operation, Some(&source), "", "")
            .await
            .unwrap_err()
            .to_string();
        assert!(err.contains("unrelated git repository"));
        assert_eq!(
            fs::read_to_string(operation.join("README.md")).unwrap(),
            "unrelated\n"
        );

        let _ = fs::remove_dir_all(base);
    });
}

#[test]
fn fallback_work_directory_requires_ufo_marker() {
    let _guard = env_lock();
    tokio::runtime::Runtime::new().unwrap().block_on(async {
        let base = std::env::temp_dir().join(format!(
            "ufo-fallback-worktree-test-{}",
            chrono::Local::now()
                .timestamp_nanos_opt()
                .unwrap_or_default()
        ));
        let operation = base.join("operation");
        ensure_work_directory(&operation, None, "", "")
            .await
            .unwrap();
        fs::create_dir_all(operation.join("assets")).unwrap();
        fs::write(operation.join("assets").join("note.txt"), "asset\n").unwrap();
        fs::write(operation.join("note.txt"), "pilot edit\n").unwrap();
        ensure_work_directory(&operation, None, "", "")
            .await
            .unwrap();
        git(&operation, &["add", "-N", "."]).await.unwrap();
        let diff = git_diff(&operation).await.unwrap();
        assert!(diff.contains("pilot edit"));
        assert!(!diff.contains("assets/note.txt"));

        let unrelated = base.join("unrelated");
        fs::create_dir_all(&unrelated).unwrap();
        init_test_git_repo(&unrelated).await.unwrap();
        fs::write(unrelated.join("README.md"), "unrelated\n").unwrap();
        git(&unrelated, &["add", "."]).await.unwrap();
        git(&unrelated, &["commit", "-q", "-m", "base"])
            .await
            .unwrap();

        let err = ensure_work_directory(&unrelated, None, "", "")
            .await
            .unwrap_err()
            .to_string();
        assert!(err.contains("not a UFO work directory"));

        let _ = fs::remove_dir_all(base);
    });
}

#[test]
fn cli_detection_uses_only_executables_on_path() {
    let _guard = env_lock();
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
    let home_bin = base.join("home-bin");
    let home_fake = home_bin.join("home-pilot");

    let result = (|| -> Result<()> {
        fs::create_dir_all(&home_bin)?;
        fs::write(&fake, "x")?;
        fs::write(&home_fake, "x")?;
        make_executable(&fake)?;
        make_executable(&home_fake)?;
        set_env("PATH", &base);
        set_env("HOME", &base);

        assert_eq!(cli_on_path("fake-pilot").as_deref(), Some(fake.as_path()));
        assert!(cli_on_path("home-pilot").is_none());
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
    let _guard = env_lock();
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
            fs::write(&path, "x")?;
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
        paths: HashMap::from([("claude".to_string(), std::env::temp_dir().join("claude"))]),
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
fn dashboard_frame_lists_rovers_and_active_runs() {
    let dashboard = Arc::new(Mutex::new(DashboardState {
        rovers: HashMap::from([(
            "rover-a".to_string(),
            RoverRuntime {
                name: "alpha".to_string(),
                fleet: "Fleet".to_string(),
                hub: "localhost:8080".to_string(),
                hub_version: "dev".to_string(),
                status: "running".to_string(),
                units: 2,
                running: HashMap::from([(
                    "run-a".to_string(),
                    RunRuntime {
                        unit: 1,
                        operation_id: "operation-a".to_string(),
                        pilot: "codex".to_string(),
                    },
                )]),
                updated_at: ts(),
            },
        )]),
        events: VecDeque::from([DashboardEvent {
            at: ts(),
            level: "accept",
            run_id: None,
            message: "rover-an accepted run-a".to_string(),
        }]),
        ..DashboardState::default()
    }));

    let frame = render_dashboard_frame(&dashboard, &test_outpost());

    assert!(frame.contains(APP_FULL_NAME));
    assert!(frame.contains("UP"));
    assert!(frame.contains("OUTPOST"));
    assert!(frame.contains("TIME"));
    assert!(frame.contains("Rovers"));
    assert!(frame.contains("Operations"));
    assert!(frame.contains("Events"));
    assert!(frame.contains("rover-a"));
    assert!(frame.contains("run-a"));
    assert!(frame.contains("localhost:8080"));
    assert!(frame.contains("Fleet"));
    assert!(frame.contains("hub"));
    assert!(frame.contains("fleet"));
    assert!(frame.contains("SLOTS"));
    let plain = plain_ansi(&frame);
    let banner_lines = plain
        .lines()
        .take(ROVER_BANNER.trim_matches('\n').lines().count())
        .collect::<Vec<_>>();
    let version_line = banner_lines
        .iter()
        .position(|line| line.contains("VERSION"))
        .expect("version metric in banner");
    let slots_line = banner_lines
        .iter()
        .position(|line| line.contains("SLOTS"))
        .expect("slots metric in banner");
    assert!(
        banner_lines
            .first()
            .is_some_and(|line| line.contains("VERSION"))
    );
    assert!(version_line < slots_line);
    assert!(slots_line > banner_lines.len() / 2);
    assert!(
        banner_lines
            .last()
            .is_some_and(|line| line.contains("ERRORS"))
    );
    assert!(plain.contains("Tab/]/PgDn next"));
    assert!(plain.contains("[/PgUp prev"));
    assert!(plain.contains("q quit"));
    assert!(frame.contains(&help_key("Tab")));
    assert!(frame.contains(&help_key("]")));
    assert!(frame.contains(&help_key("[")));
    assert!(frame.contains(&help_key("q")));
    assert!(frame.contains(env!("CARGO_PKG_VERSION")));
    assert!(frame.contains("localhost:8080"));
    assert!(frame.contains("Fleet"));
    assert!(!frame.contains("http://localhost"));
}

#[test]
fn dashboard_uses_per_rover_units_until_hub_config_arrives() {
    let selected = vec![
        ("rover-a".to_string(), sample_entry("alpha")),
        (
            "rover-b".to_string(),
            RoverEntry {
                units: 5,
                ..sample_entry("bravo")
            },
        ),
    ];

    let dashboard = new_dashboard(&selected, None);
    let state = dashboard.lock().unwrap();
    assert_eq!(state.rovers["rover-a"].units, 2);
    assert_eq!(state.rovers["rover-b"].units, 5);
}

#[test]
fn dashboard_units_override_all_rovers_when_requested() {
    let selected = vec![
        ("rover-a".to_string(), sample_entry("alpha")),
        (
            "rover-b".to_string(),
            RoverEntry {
                units: 5,
                ..sample_entry("bravo")
            },
        ),
    ];

    let dashboard = new_dashboard(&selected, Some(5));
    let state = dashboard.lock().unwrap();
    assert_eq!(state.rovers["rover-a"].units, 5);
    assert_eq!(state.rovers["rover-b"].units, 5);
}

#[test]
fn dashboard_rover_name_updates_live() {
    let selected = vec![("rover-a".to_string(), sample_entry("alpha"))];
    let dashboard = new_dashboard(&selected, None);

    dashboard_rover_name(&Some(dashboard.clone()), "rover-a", "renamed");

    let state = dashboard.lock().unwrap();
    assert_eq!(state.rovers["rover-a"].name, "renamed");
}

#[test]
fn dashboard_event_detail_shows_full_event() {
    let dashboard = Arc::new(Mutex::new(DashboardState {
        events: VecDeque::from([DashboardEvent {
            at: "2026-06-03 12:34:56 -0700".to_string(),
            level: "error",
            run_id: None,
            message: "full event detail survives outside the compact row".to_string(),
        }]),
        event_detail: true,
        ..DashboardState::default()
    }));

    let frame = render_dashboard_frame(&dashboard, &test_outpost());

    assert!(frame.contains("2026-06-03 12:34:56 -0700"));
    assert!(!frame.contains("message "));
    assert!(frame.contains("full event detail survives"));
    assert!(frame.contains("compact row"));
}

#[test]
fn dashboard_events_inline_multiline_messages() {
    let dashboard = Arc::new(Mutex::new(DashboardState {
        events: VecDeque::from([DashboardEvent {
            at: ts(),
            level: "text",
            run_id: None,
            message: "updated reaction emoji list:\nthumbs up eyes rocket\n> tsc --noEmit"
                .to_string(),
        }]),
        ..DashboardState::default()
    }));

    let frame = plain_ansi(&render_dashboard_frame(&dashboard, &test_outpost()));

    assert!(frame.contains("updated reaction emoji list: thumbs up eyes rocket > tsc --noEmit"));
    assert!(!frame.contains("list:\nthumbs"));
}

#[test]
fn dashboard_rows_fit_with_wide_log_text() {
    let dashboard = Arc::new(Mutex::new(DashboardState {
        rovers: HashMap::from([(
            "rover-a-full-id".to_string(),
            RoverRuntime {
                name: "alpha".to_string(),
                fleet: "Fleet".to_string(),
                hub: "localhost:8080".to_string(),
                hub_version: "dev".to_string(),
                status: "running".to_string(),
                units: 1,
                running: HashMap::from([(
                    "run-a-full-id".to_string(),
                    RunRuntime {
                        unit: 1,
                        operation_id: "operation-a".to_string(),
                        pilot: "codex".to_string(),
                    },
                )]),
                updated_at: ts(),
            },
        )]),
        events: VecDeque::from([DashboardEvent {
            at: ts(),
            level: "text",
            run_id: Some("run-a-full-id".to_string()),
            message:
                "tool 输出包含全角字符和很长很长的 synthetic/path/example-output-file-name.txt"
                    .to_string(),
        }]),
        ..DashboardState::default()
    }));

    let frame = render_dashboard_frame(&dashboard, &test_outpost());

    for line in frame.lines() {
        assert!(
            visible_len(line) <= TUI_MAX_WIDTH,
            "line exceeds dashboard width: {line:?}"
        );
    }
}

#[test]
fn dashboard_operation_opens_run_detail() {
    let operation_id = "operation-with-a-long-id-that-needs-detail";
    let dashboard = Arc::new(Mutex::new(DashboardState {
        rovers: HashMap::from([(
            "rover-a-full-id".to_string(),
            RoverRuntime {
                name: "alpha".to_string(),
                fleet: "Fleet".to_string(),
                hub: "localhost:8080".to_string(),
                hub_version: "dev".to_string(),
                status: "running".to_string(),
                units: 2,
                running: HashMap::from([(
                    "run-a-full-id".to_string(),
                    RunRuntime {
                        unit: 1,
                        operation_id: operation_id.to_string(),
                        pilot: "codex".to_string(),
                    },
                )]),
                updated_at: ts(),
            },
        )]),
        ..DashboardState::default()
    }));

    handle_dashboard_keys(&dashboard, b"\t\r");
    let frame = render_dashboard_frame(&dashboard, &test_outpost());

    assert!(frame.contains("operation"));
    assert!(frame.contains(operation_id));
    assert!(frame.contains("run-a-full-id"));
    assert!(frame.contains("rover-a-full-id"));
}

#[test]
fn dashboard_operation_run_detail_survives_empty_active_rows() {
    let dashboard = Arc::new(Mutex::new(DashboardState {
        focus: DashboardFocus::Operations,
        run_detail: Some(DashboardRunDetail {
            rover_id: "rover-a".to_string(),
            unit: 1,
            run_id: "run-a".to_string(),
            operation_id: "operation-a".to_string(),
            pilot: "codex".to_string(),
        }),
        ..DashboardState::default()
    }));

    let frame = plain_ansi(&render_dashboard_frame(&dashboard, &test_outpost()));

    assert!(frame.contains("‹ operation-a"));
    assert!(frame.contains("run-a"));
    assert!(frame.contains("rover-a"));
}

#[test]
fn dashboard_operation_opens_run_logs() {
    let dashboard = Arc::new(Mutex::new(DashboardState {
        rovers: HashMap::from([(
            "rover-a-full-id".to_string(),
            RoverRuntime {
                name: "alpha".to_string(),
                fleet: "Fleet".to_string(),
                hub: "localhost:8080".to_string(),
                hub_version: "dev".to_string(),
                status: "running".to_string(),
                units: 1,
                running: HashMap::from([(
                    "run-a-full-id".to_string(),
                    RunRuntime {
                        unit: 1,
                        operation_id: "operation-a".to_string(),
                        pilot: "codex".to_string(),
                    },
                )]),
                updated_at: ts(),
            },
        )]),
        events: VecDeque::from([DashboardEvent {
            at: ts(),
            level: "log",
            run_id: Some("run-a-full-id".to_string()),
            message: "operation log line".to_string(),
        }]),
        ..DashboardState::default()
    }));

    handle_dashboard_keys(&dashboard, b"\t\r");
    let frame = render_dashboard_frame(&dashboard, &test_outpost());

    assert!(frame.contains("run-a-full-id"));
    assert!(frame.contains("operation log line"));
}

#[test]
fn dashboard_rover_detail_shows_full_rover() {
    let dashboard = Arc::new(Mutex::new(DashboardState {
        rovers: HashMap::from([(
            "rover-a-full-id".to_string(),
            RoverRuntime {
                name: "alpha".to_string(),
                fleet: "Fleet".to_string(),
                hub: "localhost:8080".to_string(),
                hub_version: "dev".to_string(),
                status: "running".to_string(),
                units: 2,
                running: HashMap::new(),
                updated_at: ts(),
            },
        )]),
        ..DashboardState::default()
    }));

    handle_dashboard_keys(&dashboard, b"\r");
    let frame = render_dashboard_frame(&dashboard, &test_outpost());
    let plain = plain_ansi(&frame);

    assert!(frame.contains("localhost:8080"));
    assert!(frame.contains("Fleet"));
    assert!(frame.contains("slot 1"));
    assert!(frame.contains("slot 2"));
    assert!(frame.contains("alpha"));
    for line in plain
        .lines()
        .filter(|l| l.contains("slot ") || l.contains("Fleet"))
    {
        assert!(!line.trim_end().ends_with("..."), "row truncated: {line:?}");
    }
}

#[test]
fn dashboard_rover_unit_detail_opens_run_logs() {
    let dashboard = Arc::new(Mutex::new(DashboardState {
        rovers: HashMap::from([(
            "rover-a-full-id".to_string(),
            RoverRuntime {
                name: "alpha".to_string(),
                fleet: "Fleet".to_string(),
                hub: "localhost:8080".to_string(),
                hub_version: "dev".to_string(),
                status: "running".to_string(),
                units: 1,
                running: HashMap::from([(
                    "run-a-full-id".to_string(),
                    RunRuntime {
                        unit: 1,
                        operation_id: "operation-a".to_string(),
                        pilot: "codex".to_string(),
                    },
                )]),
                updated_at: ts(),
            },
        )]),
        events: VecDeque::from([DashboardEvent {
            at: ts(),
            level: "log",
            run_id: Some("run-a-full-id".to_string()),
            message: "unit log line".to_string(),
        }]),
        ..DashboardState::default()
    }));

    handle_dashboard_keys(&dashboard, b"\r\r");
    let frame = render_dashboard_frame(&dashboard, &test_outpost());

    assert!(frame.contains("run-a-fu"));
    assert!(frame.contains("unit log line"));
}

#[test]
fn dashboard_allocates_lowest_free_unit() {
    let mut rover = RoverRuntime {
        name: "alpha".to_string(),
        fleet: "Fleet".to_string(),
        hub: "localhost:8080".to_string(),
        hub_version: "dev".to_string(),
        status: "running".to_string(),
        units: 2,
        running: HashMap::from([(
            "run-a".to_string(),
            RunRuntime {
                unit: 1,
                operation_id: "operation-a".to_string(),
                pilot: "codex".to_string(),
            },
        )]),
        updated_at: ts(),
    };

    assert_eq!(next_rover_unit(&rover), 2);

    rover.running.insert(
        "run-b".to_string(),
        RunRuntime {
            unit: 2,
            operation_id: "operation-b".to_string(),
            pilot: "codex".to_string(),
        },
    );

    assert_eq!(next_rover_unit(&rover), 3);
}

#[test]
fn dashboard_event_detail_freezes_selected_event() {
    let dashboard = Arc::new(Mutex::new(DashboardState {
        events: (0..EVENT_BUFFER_ROWS)
            .map(|i| DashboardEvent {
                at: ts(),
                level: "info",
                run_id: None,
                message: format!("event-{i}"),
            })
            .collect(),
        selected_event: EVENT_BUFFER_ROWS - 1,
        ..DashboardState::default()
    }));

    handle_dashboard_keys(&dashboard, b"\t\n");
    dashboard_event(&Some(dashboard.clone()), "info", "new event".to_string());

    let frame = render_dashboard_frame(&dashboard, &test_outpost());

    assert!(frame.contains("event-0"));
}

#[test]
fn dashboard_event_rows_fit_terminal_height() {
    assert_eq!(event_rows_for_height(60, 1, 0), EVENT_ROWS);
    assert_eq!(event_rows_for_height(23, 1, 1), 1);
    assert_eq!(event_rows_for_height(65, 13, 10), EVENT_ROWS - 1);
}

#[test]
fn dashboard_home_windows_many_rovers_units_and_operations() {
    let (rover_rows, operation_rows) = fit_dashboard_body_rows(36, 89, 81, true, true);

    assert_eq!(rover_rows + operation_rows, 12);
    assert!(visible_window(80, 79, operation_rows - 1).contains(&79));
}

#[test]
fn dashboard_uptime_formats_elapsed_time() {
    assert_eq!(format_duration(Duration::from_secs(0)), "00:00:00");
    assert_eq!(format_duration(Duration::from_secs(65)), "00:01:05");
    assert_eq!(format_duration(Duration::from_secs(3661)), "01:01:01");
    assert_eq!(dashboard_uptime(Some(100), 100), "00:00:00");
    assert_eq!(dashboard_uptime(Some(100), 165), "00:01:05");
    assert_eq!(dashboard_uptime(Some(200), 100), "00:00:00");
}

#[test]
fn dashboard_keys_select_and_toggle_event_detail() {
    let dashboard = Arc::new(Mutex::new(DashboardState {
        events: VecDeque::from([
            DashboardEvent {
                at: ts(),
                level: "start",
                run_id: None,
                message: "first".to_string(),
            },
            DashboardEvent {
                at: ts(),
                level: "accept",
                run_id: None,
                message: "second".to_string(),
            },
        ]),
        ..DashboardState::default()
    }));

    handle_dashboard_keys(&dashboard, b"\t\x1b[B\x1b[C\x1b[D");
    let state = dashboard.lock().unwrap();

    assert_eq!(state.selected_event, 1);
    assert!(!state.event_detail);
}

#[test]
fn dashboard_tab_skips_empty_operations() {
    let dashboard = Arc::new(Mutex::new(DashboardState {
        events: VecDeque::from([DashboardEvent {
            at: ts(),
            level: "info",
            run_id: None,
            message: "event".to_string(),
        }]),
        ..DashboardState::default()
    }));

    handle_dashboard_keys(&dashboard, b"\t");
    let state = dashboard.lock().unwrap();

    assert!(state.focus == DashboardFocus::Events);
}

#[test]
fn dashboard_tab_wraps_focus() {
    let dashboard = Arc::new(Mutex::new(DashboardState {
        events: VecDeque::from([DashboardEvent {
            at: ts(),
            level: "info",
            run_id: None,
            message: "event".to_string(),
        }]),
        ..DashboardState::default()
    }));

    handle_dashboard_keys(&dashboard, b"\t\t");
    let state = dashboard.lock().unwrap();

    assert!(state.focus == DashboardFocus::Rovers);
}

#[test]
fn dashboard_page_keys_move_focus_both_directions() {
    let dashboard = Arc::new(Mutex::new(DashboardState {
        rovers: HashMap::from([(
            "rover-a".to_string(),
            RoverRuntime {
                name: "alpha".to_string(),
                fleet: "Fleet".to_string(),
                hub: "localhost:8080".to_string(),
                hub_version: "dev".to_string(),
                status: "running".to_string(),
                units: 2,
                running: HashMap::from([(
                    "run-a".to_string(),
                    RunRuntime {
                        unit: 1,
                        operation_id: "operation-a".to_string(),
                        pilot: "codex".to_string(),
                    },
                )]),
                updated_at: ts(),
            },
        )]),
        events: VecDeque::from([DashboardEvent {
            at: ts(),
            level: "info",
            run_id: None,
            message: "event".to_string(),
        }]),
        ..DashboardState::default()
    }));

    handle_dashboard_keys(&dashboard, b"\x1b[6~");
    assert!(dashboard.lock().unwrap().focus == DashboardFocus::Operations);

    handle_dashboard_keys(&dashboard, b"\x1b[6~");
    assert!(dashboard.lock().unwrap().focus == DashboardFocus::Events);

    handle_dashboard_keys(&dashboard, b"\x1b[6~");
    assert!(dashboard.lock().unwrap().focus == DashboardFocus::Events);

    handle_dashboard_keys(&dashboard, b"\x1b[5~");
    assert!(dashboard.lock().unwrap().focus == DashboardFocus::Operations);

    handle_dashboard_keys(&dashboard, b"]");
    assert!(dashboard.lock().unwrap().focus == DashboardFocus::Events);

    handle_dashboard_keys(&dashboard, b"[");
    assert!(dashboard.lock().unwrap().focus == DashboardFocus::Operations);

    handle_dashboard_keys(&dashboard, b"[");
    assert!(dashboard.lock().unwrap().focus == DashboardFocus::Rovers);

    handle_dashboard_keys(&dashboard, b"[");
    assert!(dashboard.lock().unwrap().focus == DashboardFocus::Rovers);
}

#[test]
fn dashboard_buffers_split_escape_sequences() {
    let dashboard = Arc::new(Mutex::new(DashboardState {
        rovers: HashMap::from([(
            "rover-a".to_string(),
            RoverRuntime {
                name: "alpha".to_string(),
                fleet: "Fleet".to_string(),
                hub: "localhost:8080".to_string(),
                hub_version: "dev".to_string(),
                status: "running".to_string(),
                units: 2,
                running: HashMap::from([(
                    "run-a".to_string(),
                    RunRuntime {
                        unit: 1,
                        operation_id: "operation-a".to_string(),
                        pilot: "codex".to_string(),
                    },
                )]),
                updated_at: ts(),
            },
        )]),
        ..DashboardState::default()
    }));
    let mut pending = vec![27];

    handle_dashboard_key_buffer(&dashboard, &mut pending, false);
    assert!(dashboard.lock().unwrap().focus == DashboardFocus::Rovers);
    assert_eq!(pending, vec![27]);

    pending.extend_from_slice(b"[B");
    handle_dashboard_key_buffer(&dashboard, &mut pending, false);
    assert_eq!(dashboard.lock().unwrap().selected_rover, 0);
    assert!(pending.is_empty());
}

#[test]
fn dashboard_q_requires_confirmation_and_escape_only_closes_detail() {
    let dashboard = Arc::new(Mutex::new(DashboardState {
        events: VecDeque::from([DashboardEvent {
            at: ts(),
            level: "info",
            run_id: None,
            message: "event".to_string(),
        }]),
        ..DashboardState::default()
    }));

    handle_dashboard_keys(&dashboard, b"\t\r\x1b");
    {
        let state = dashboard.lock().unwrap();
        assert!(!state.event_detail);
        assert!(!state.quit);
    }

    handle_dashboard_keys(&dashboard, b"q");
    {
        let state = dashboard.lock().unwrap();
        assert!(state.quit_confirm);
        assert!(!state.quit);
    }

    handle_dashboard_keys(&dashboard, b"n");
    {
        let state = dashboard.lock().unwrap();
        assert!(!state.quit_confirm);
        assert!(!state.quit);
    }

    handle_dashboard_keys(&dashboard, b"qy");
    let state = dashboard.lock().unwrap();

    assert!(state.quit);
}

#[test]
fn ctrl_c_requests_confirmation_in_dashboard() {
    let dashboard = Some(Arc::new(Mutex::new(DashboardState::default())));

    assert!(!request_quit(&dashboard));
    {
        let state = dashboard.as_ref().unwrap().lock().unwrap();
        assert!(state.quit_confirm);
        assert!(!state.quit);
    }

    assert!(request_quit(&None));
}

#[test]
fn dashboard_empty_operations_skip_table_header() {
    let dashboard = Arc::new(Mutex::new(DashboardState {
        rovers: HashMap::from([(
            "rover-a".to_string(),
            RoverRuntime {
                name: "alpha".to_string(),
                fleet: "Fleet".to_string(),
                hub: "localhost:8080".to_string(),
                hub_version: "dev".to_string(),
                status: "ready".to_string(),
                units: 2,
                running: HashMap::new(),
                updated_at: ts(),
            },
        )]),
        events: VecDeque::new(),
        ..DashboardState::default()
    }));

    let frame = render_dashboard_frame(&dashboard, &test_outpost());

    assert!(frame.contains(&dim("  none")));
    assert!(!frame.contains("pilot"));
}

#[test]
fn truncate_field_keeps_cells_bounded() {
    assert_eq!(truncate_field("abcdef", 5), "ab...");
    assert_eq!(truncate_field("abc", 5), "abc");
    assert_eq!(visible_len(&truncate_field("宽字符测试", 5)), 5);
}

#[test]
fn pilot_registry_includes_default_pilots() {
    assert_eq!(
        PILOTS.iter().map(|pilot| pilot.kind).collect::<Vec<_>>(),
        vec![
            "claude",
            "codex",
            "antigravity",
            "grok",
            "cursor",
            "copilot",
            "opencode",
            "amp",
            "openclaw",
            "hermes",
            "pi",
            "kilo",
            "mastracode",
            "devin",
            "kiro",
            "kimi",
            "codebuddy",
            "qoder",
            "trae",
        ]
    );
    assert!(find_pilot("missing").is_none());
}

#[test]
fn antigravity_command_uses_print_mode() {
    let run = sample_accepted_run("antigravity");
    let cmd = antigravity_command(Path::new("agy"), &run, "do it", Path::new("/tmp"));
    let args = cmd
        .as_std()
        .get_args()
        .map(|arg| arg.to_string_lossy().into_owned())
        .collect::<Vec<_>>();

    assert_eq!(
        args,
        vec![
            "--conversation",
            "session",
            "--dangerously-skip-permissions",
            "-p",
            "do it",
            "--output-format",
            "json",
        ]
    );
}

#[test]
fn copilot_command_uses_prompt_mode() {
    let run = sample_accepted_run("copilot");
    let cmd = copilot_command(Path::new("copilot"), &run, "do it", Path::new("/tmp"));
    let args = cmd
        .as_std()
        .get_args()
        .map(|arg| arg.to_string_lossy().into_owned())
        .collect::<Vec<_>>();

    assert_eq!(
        args,
        vec![
            "--session-id",
            "session",
            "-p",
            "do it",
            "--yolo",
            "--no-ask-user",
        ]
    );
}

#[test]
fn plain_text_pilots_use_non_interactive_prompt_mode() {
    let run = sample_accepted_run("");
    let hermes_usage = hermes_usage_path("run").to_string_lossy().into_owned();

    for (args, expected) in [
        (
            hermes_command(Path::new("hermes"), &run, "do it", Path::new("/tmp"))
                .as_std()
                .get_args()
                .map(|arg| arg.to_string_lossy().into_owned())
                .collect::<Vec<_>>(),
            vec![
                "--resume",
                "session",
                "--yolo",
                "-z",
                "do it",
                "--usage-file",
                hermes_usage.as_str(),
            ],
        ),
        (
            cursor_agent_command(Path::new("cursor-agent"), &run, "do it", Path::new("/tmp"))
                .as_std()
                .get_args()
                .map(|arg| arg.to_string_lossy().into_owned())
                .collect::<Vec<_>>(),
            vec![
                "--print",
                "--output-format",
                "json",
                "--force",
                "--trust",
                "--resume",
                "session",
                "do it",
            ],
        ),
        (
            amp_command(Path::new("amp"), &run, "do it", Path::new("/tmp"))
                .as_std()
                .get_args()
                .map(|arg| arg.to_string_lossy().into_owned())
                .collect::<Vec<_>>(),
            vec!["--execute", "do it"],
        ),
        (
            pi_command(Path::new("pi"), &run, "do it", Path::new("/tmp"))
                .as_std()
                .get_args()
                .map(|arg| arg.to_string_lossy().into_owned())
                .collect::<Vec<_>>(),
            vec!["--session", "session", "--approve", "--print", "do it"],
        ),
        (
            kimi_command(Path::new("kimi"), &run, "do it", Path::new("/tmp"))
                .as_std()
                .get_args()
                .map(|arg| arg.to_string_lossy().into_owned())
                .collect::<Vec<_>>(),
            vec!["--continue", "-p", "do it", "--output-format", "text"],
        ),
        (
            kiro_command(Path::new("kiro-cli"), &run, "do it", Path::new("/tmp"))
                .as_std()
                .get_args()
                .map(|arg| arg.to_string_lossy().into_owned())
                .collect::<Vec<_>>(),
            vec![
                "chat",
                "--no-interactive",
                "--trust-all-tools",
                "--wrap",
                "never",
                "--resume-id",
                "session",
                "do it",
            ],
        ),
        (
            grok_command(Path::new("grok"), &run, "do it", Path::new("/tmp"))
                .as_std()
                .get_args()
                .map(|arg| arg.to_string_lossy().into_owned())
                .collect::<Vec<_>>(),
            vec![
                "--resume",
                "session",
                "--always-approve",
                "--output-format",
                "json",
                "-p",
                "do it",
            ],
        ),
        (
            codebuddy_command(Path::new("codebuddy"), &run, "do it", Path::new("/tmp"))
                .as_std()
                .get_args()
                .map(|arg| arg.to_string_lossy().into_owned())
                .collect::<Vec<_>>(),
            vec!["-p", "do it"],
        ),
        (
            qoder_command(Path::new("qodercli"), &run, "do it", Path::new("/tmp"))
                .as_std()
                .get_args()
                .map(|arg| arg.to_string_lossy().into_owned())
                .collect::<Vec<_>>(),
            vec![
                "-r",
                "session",
                "--yolo",
                "-p",
                "do it",
                "--output-format",
                "text",
            ],
        ),
        (
            trae_command(Path::new("trae-cli"), &run, "do it", Path::new("/tmp"))
                .as_std()
                .get_args()
                .map(|arg| arg.to_string_lossy().into_owned())
                .collect::<Vec<_>>(),
            vec!["run", "do it", "--working-dir", "/tmp"],
        ),
        (
            devin_command(Path::new("devin"), &run, "do it", Path::new("/tmp"))
                .as_std()
                .get_args()
                .map(|arg| arg.to_string_lossy().into_owned())
                .collect::<Vec<_>>(),
            vec![
                "--continue",
                "--permission-mode",
                "dangerous",
                "--respect-workspace-trust",
                "false",
                "--print",
                "do it",
            ],
        ),
        (
            kilo_command(Path::new("kilo"), &run, "do it", Path::new("/tmp"))
                .as_std()
                .get_args()
                .map(|arg| arg.to_string_lossy().into_owned())
                .collect::<Vec<_>>(),
            vec!["run", "--auto", "--dir", "/tmp", "--continue", "do it"],
        ),
        (
            mastracode_command(Path::new("mastracode"), &run, "do it", Path::new("/tmp"))
                .as_std()
                .get_args()
                .map(|arg| arg.to_string_lossy().into_owned())
                .collect::<Vec<_>>(),
            vec![
                "--thread", "session", "--prompt", "do it", "--output", "json",
            ],
        ),
    ] {
        assert_eq!(args, expected);
    }
}

#[test]
fn mastracode_first_run_creates_a_thread_instead_of_resuming() {
    let mut run = sample_accepted_run("mastracode");
    run.session_id.clear();
    let args = mastracode_command(Path::new("mastracode"), &run, "do it", Path::new("/tmp"))
        .as_std()
        .get_args()
        .map(|arg| arg.to_string_lossy().into_owned())
        .collect::<Vec<_>>();
    assert_eq!(args, vec!["--prompt", "do it", "--output", "json"]);
}

#[test]
fn mastracode_result_reads_thread_and_text() {
    assert_eq!(
        parse_mastracode_result(r#"{"threadId":"th_1","text":"done"}"#),
        ("th_1".to_string(), "done".to_string())
    );
    assert_eq!(
        parse_mastracode_result("plain"),
        (String::new(), "plain".to_string())
    );
}

#[test]
fn cursor_result_reads_session_and_text() {
    assert_eq!(
        parse_cursor_result(r#"{"session_id":"chat_1","result":"done"}"#),
        ("chat_1".to_string(), "done".to_string())
    );
}

#[test]
fn antigravity_result_reads_conversation_and_text() {
    assert_eq!(
        parse_antigravity_result(r#"{"conversation_id":"conv_1","response":"done"}"#),
        ("conv_1".to_string(), "done".to_string())
    );
}

#[test]
fn grok_result_reads_session_and_text() {
    assert_eq!(
        parse_grok_result(r#"{"sessionId":"abc123","text":"done"}"#),
        ("abc123".to_string(), "done".to_string())
    );
}

#[test]
fn cwd_continue_pilots_persist_an_operation_session_key() {
    let mut run = sample_accepted_run("kilo");
    run.session_id.clear();
    assert_eq!(
        parse_plain_pilot_result("kilo", &run, "done"),
        ("ufo-operation".to_string(), "done".to_string())
    );
    assert_eq!(
        parse_plain_pilot_result("devin", &run, "done"),
        ("ufo-operation".to_string(), "done".to_string())
    );
    assert_eq!(
        parse_plain_pilot_result("kimi", &run, "done"),
        ("ufo-operation".to_string(), "done".to_string())
    );
}

#[test]
fn kilo_and_devin_first_runs_do_not_invent_session_flags() {
    let mut kilo = sample_accepted_run("kilo");
    kilo.session_id.clear();
    let kilo_args = kilo_command(Path::new("kilo"), &kilo, "do it", Path::new("/tmp"))
        .as_std()
        .get_args()
        .map(|arg| arg.to_string_lossy().into_owned())
        .collect::<Vec<_>>();
    assert_eq!(kilo_args, vec!["run", "--auto", "--dir", "/tmp", "do it"]);

    let mut devin = sample_accepted_run("devin");
    devin.session_id.clear();
    let devin_args = devin_command(Path::new("devin"), &devin, "do it", Path::new("/tmp"))
        .as_std()
        .get_args()
        .map(|arg| arg.to_string_lossy().into_owned())
        .collect::<Vec<_>>();
    assert_eq!(
        devin_args,
        vec![
            "--permission-mode",
            "dangerous",
            "--respect-workspace-trust",
            "false",
            "--print",
            "do it",
        ]
    );
}

#[test]
fn hermes_result_reads_usage_session() {
    let mut run = sample_accepted_run("hermes");
    run.session_id.clear();
    let path = hermes_usage_path(&run.id);
    fs::write(&path, r#"{"session_id":"20260225_143052_a1b2c3"}"#).unwrap();
    assert_eq!(
        parse_hermes_result(&run, "Paris."),
        ("20260225_143052_a1b2c3".to_string(), "Paris.".to_string())
    );
    assert!(!path.exists());
}

#[test]
fn opencode_command_uses_documented_auto_flag() {
    let run = sample_accepted_run("opencode");
    let args = opencode_command(Path::new("opencode"), &run, "do it", Path::new("/tmp"))
        .as_std()
        .get_args()
        .map(|arg| arg.to_string_lossy().into_owned())
        .collect::<Vec<_>>();
    assert_eq!(
        args,
        vec![
            "run",
            "--auto",
            "--dir",
            "/tmp",
            "--session",
            "session",
            "do it"
        ]
    );
}

#[test]
fn copilot_first_run_mints_a_session_uuid() {
    let mut run = sample_accepted_run("copilot");
    run.session_id.clear();
    let minted = operation_session_uuid(&run);
    let args = copilot_command(Path::new("copilot"), &run, "do it", Path::new("/tmp"))
        .as_std()
        .get_args()
        .map(|arg| arg.to_string_lossy().into_owned())
        .collect::<Vec<_>>();
    assert_eq!(
        args,
        vec![
            "--session-id",
            &minted,
            "-p",
            "do it",
            "--yolo",
            "--no-ask-user",
        ]
    );
    assert_eq!(minted.len(), 36);
    assert_eq!(minted.chars().filter(|c| *c == '-').count(), 4);
}

#[test]
fn kimi_first_run_uses_prompt_mode_without_a_session() {
    let mut run = sample_accepted_run("kimi");
    run.session_id.clear();
    let args = kimi_command(Path::new("kimi"), &run, "do it", Path::new("/tmp"))
        .as_std()
        .get_args()
        .map(|arg| arg.to_string_lossy().into_owned())
        .collect::<Vec<_>>();
    assert_eq!(args, vec!["-p", "do it", "--output-format", "text"]);
}

#[test]
fn grok_first_run_lets_the_cli_create_the_session() {
    let mut run = sample_accepted_run("grok");
    run.session_id.clear();
    let args = grok_command(Path::new("grok"), &run, "do it", Path::new("/tmp"))
        .as_std()
        .get_args()
        .map(|arg| arg.to_string_lossy().into_owned())
        .collect::<Vec<_>>();
    assert_eq!(
        args,
        vec!["--always-approve", "--output-format", "json", "-p", "do it"]
    );
}

#[test]
fn kiro_first_run_stays_on_documented_headless_flags() {
    let mut run = sample_accepted_run("kiro");
    run.session_id.clear();
    let args = kiro_command(Path::new("kiro-cli"), &run, "do it", Path::new("/tmp"))
        .as_std()
        .get_args()
        .map(|arg| arg.to_string_lossy().into_owned())
        .collect::<Vec<_>>();
    assert_eq!(
        args,
        vec![
            "chat",
            "--no-interactive",
            "--trust-all-tools",
            "--wrap",
            "never",
            "do it",
        ]
    );
}

#[test]
fn antigravity_first_run_skips_conversation_resume() {
    let mut run = sample_accepted_run("antigravity");
    run.session_id.clear();
    let args = antigravity_command(Path::new("agy"), &run, "do it", Path::new("/tmp"))
        .as_std()
        .get_args()
        .map(|arg| arg.to_string_lossy().into_owned())
        .collect::<Vec<_>>();
    assert_eq!(
        args,
        vec![
            "--dangerously-skip-permissions",
            "-p",
            "do it",
            "--output-format",
            "json",
        ]
    );
}

#[test]
fn pi_first_run_does_not_invent_a_session() {
    let mut run = sample_accepted_run("pi");
    run.session_id.clear();
    let args = pi_command(Path::new("pi"), &run, "do it", Path::new("/tmp"))
        .as_std()
        .get_args()
        .map(|arg| arg.to_string_lossy().into_owned())
        .collect::<Vec<_>>();
    assert_eq!(args, vec!["--approve", "--print", "do it"]);
}

#[test]
fn hermes_first_run_writes_usage_and_skips_resume() {
    let mut run = sample_accepted_run("hermes");
    run.session_id.clear();
    let usage = hermes_usage_path("run").to_string_lossy().into_owned();
    let args = hermes_command(Path::new("hermes"), &run, "do it", Path::new("/tmp"))
        .as_std()
        .get_args()
        .map(|arg| arg.to_string_lossy().into_owned())
        .collect::<Vec<_>>();
    assert_eq!(
        args,
        vec!["--yolo", "-z", "do it", "--usage-file", usage.as_str()]
    );
}

#[test]
fn codex_resume_uses_the_same_exec_sandbox_flags() {
    let run = sample_accepted_run("codex");
    let resume = codex_command(Path::new("codex"), &run, "do it", Path::new("/tmp"))
        .as_std()
        .get_args()
        .map(|arg| arg.to_string_lossy().into_owned())
        .collect::<Vec<_>>();
    let mut first = sample_accepted_run("codex");
    first.session_id.clear();
    let start = codex_command(Path::new("codex"), &first, "do it", Path::new("/tmp"))
        .as_std()
        .get_args()
        .map(|arg| arg.to_string_lossy().into_owned())
        .collect::<Vec<_>>();
    assert_eq!(
        start,
        vec![
            "exec",
            "-s",
            "danger-full-access",
            "--skip-git-repo-check",
            "--json",
            "do it",
        ]
    );
    assert_eq!(
        resume,
        vec![
            "exec",
            "-s",
            "danger-full-access",
            "--skip-git-repo-check",
            "--json",
            "resume",
            "session",
            "do it",
        ]
    );
}

#[test]
fn config_roundtrip_preserves_multiple_rover_enrollments() {
    let _guard = env_lock();
    let base = std::env::temp_dir().join(format!(
        "ufo-rover-test-{}",
        chrono::Local::now()
            .timestamp_nanos_opt()
            .unwrap_or_default()
    ));
    let path = base.join("rovers.json");
    set_env("UFO_ROVER_CONFIG", &path);

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

    remove_env("UFO_ROVER_CONFIG");
    let _ = fs::remove_dir_all(base);
    result.unwrap();
}

#[test]
fn remove_entry_deletes_only_that_rover() {
    let _guard = env_lock();
    let base = std::env::temp_dir().join(format!(
        "ufo-rover-remove-entry-test-{}",
        chrono::Local::now()
            .timestamp_nanos_opt()
            .unwrap_or_default()
    ));
    let path = base.join("rovers.json");
    set_env("UFO_ROVER_CONFIG", &path);

    let result = (|| -> Result<()> {
        save_entry("rover-a", &sample_entry("alpha"))?;
        save_entry("rover-b", &sample_entry("beta"))?;

        let removed = remove_entry("rover-a")?;
        let cfg = load_config()?;
        assert_eq!(removed.map(|entry| entry.name), Some("alpha".to_string()));
        assert!(!cfg.rovers.contains_key("rover-a"));
        assert!(cfg.rovers.contains_key("rover-b"));

        let err = forget_rejected_rover("rover-b", "http://hub");
        assert!(err.to_string().contains("removed local enrollment"));
        assert!(load_config()?.rovers.is_empty());
        Ok(())
    })();

    remove_env("UFO_ROVER_CONFIG");
    let _ = fs::remove_dir_all(base);
    result.unwrap();
}

#[test]
fn sync_rover_name_updates_local_config() {
    let _guard = env_lock();
    let base = std::env::temp_dir().join(format!(
        "ufo-rover-rename-test-{}",
        chrono::Local::now()
            .timestamp_nanos_opt()
            .unwrap_or_default()
    ));
    let path = base.join("rovers.json");
    set_env("UFO_ROVER_CONFIG", &path);

    let result = (|| -> Result<()> {
        let mut entry = sample_entry("old");
        save_entry("rover-a", &entry)?;
        sync_rover_name("rover-a", &mut entry, "new")?;

        let cfg = load_config()?;
        assert_eq!(entry.name, "new");
        assert_eq!(cfg.rovers["rover-a"].name, "new");
        Ok(())
    })();

    remove_env("UFO_ROVER_CONFIG");
    let _ = fs::remove_dir_all(base);
    result.unwrap();
}

#[test]
fn sync_rover_units_updates_local_config() {
    let _guard = env_lock();
    let base = std::env::temp_dir().join(format!(
        "ufo-rover-units-test-{}",
        chrono::Local::now()
            .timestamp_nanos_opt()
            .unwrap_or_default()
    ));
    let path = base.join("rovers.json");
    set_env("UFO_ROVER_CONFIG", &path);

    let result = (|| -> Result<()> {
        save_entry("rover-a", &sample_entry("alpha"))?;
        sync_rover_units_by_id("rover-a", 7);

        let cfg = load_config()?;
        assert_eq!(cfg.rovers["rover-a"].units, 7);
        Ok(())
    })();

    remove_env("UFO_ROVER_CONFIG");
    let _ = fs::remove_dir_all(base);
    result.unwrap();
}

#[test]
fn sync_rover_tags_updates_local_config() {
    let _guard = env_lock();
    let base = std::env::temp_dir().join(format!(
        "ufo-rover-tags-test-{}",
        chrono::Local::now()
            .timestamp_nanos_opt()
            .unwrap_or_default()
    ));
    let path = base.join("rovers.json");
    set_env("UFO_ROVER_CONFIG", &path);

    let result = (|| -> Result<()> {
        save_entry("rover-a", &sample_entry("alpha"))?;
        sync_rover_tags_by_id("rover-a", &["region:lab".to_string()]);

        let cfg = load_config()?;
        assert_eq!(cfg.rovers["rover-a"].tags, vec!["region:lab"]);
        Ok(())
    })();

    remove_env("UFO_ROVER_CONFIG");
    let _ = fs::remove_dir_all(base);
    result.unwrap();
}

#[test]
fn config_file_lock_rejects_concurrent_writer() {
    let _guard = env_lock();
    let base = std::env::temp_dir().join(format!(
        "ufo-rover-lock-test-{}",
        chrono::Local::now()
            .timestamp_nanos_opt()
            .unwrap_or_default()
    ));
    let path = base.join("rovers.json");
    set_env("UFO_ROVER_CONFIG", &path);

    let result = (|| -> Result<()> {
        let _lock = lock_config_file(&path)?;
        let err = save_entry("rover-a", &sample_entry("alpha")).unwrap_err();
        assert!(err.to_string().contains("locked by another process"));
        Ok(())
    })();

    remove_env("UFO_ROVER_CONFIG");
    let _ = fs::remove_dir_all(base);
    result.unwrap();
}

#[test]
fn malformed_config_returns_an_error() {
    let _guard = env_lock();
    let base = std::env::temp_dir().join(format!(
        "ufo-rover-bad-config-{}",
        chrono::Local::now()
            .timestamp_nanos_opt()
            .unwrap_or_default()
    ));
    let path = base.join("rovers.json");
    fs::create_dir_all(&base).unwrap();
    fs::write(&path, "{not-json").unwrap();
    set_env("UFO_ROVER_CONFIG", &path);

    let err = match load_config() {
        Ok(_) => panic!("malformed config should not be ignored"),
        Err(err) => err,
    };

    remove_env("UFO_ROVER_CONFIG");
    let _ = fs::remove_dir_all(base);
    assert!(err.to_string().contains("parse rover config"));
}
