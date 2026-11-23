use agent_watcher::{
    Config, Harness, Snapshotter,
    scanner::{ProcessMeta, ProcessSnapshot, SessionState},
};
use serde_json::json;
use std::{
    fs, io,
    path::Path,
    sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    },
};
use tokio_util::sync::CancellationToken;

fn temp() -> tempfile::TempDir {
    fs::create_dir_all(".tmp").unwrap();
    tempfile::tempdir_in(".tmp").unwrap()
}
fn write(path: &Path, value: serde_json::Value) {
    fs::create_dir_all(path.parent().unwrap()).unwrap();
    fs::write(path, format!("{value}\n")).unwrap();
}
fn config(root: &Path, calls: Arc<AtomicUsize>) -> Config {
    let root = fs::canonicalize(root).unwrap();
    let cwd = root.clone();
    Config {
        claude_sessions_dirs: vec![root.join("claude")],
        claude_projects_dir: root.join("projects"),
        pi_sessions_dir: root.join("pi"),
        codex_sessions_dir: root.join("codex"),
        opencode_db_path: root.join("opencode.db"),
        cursor_path: root.join("do-not-create/cursors"),
        opencode_checkpoint_path: root.join("do-not-create/checkpoints"),
        session_state_path: root.join("do-not-create/state"),
        process_snapshot_provider: Arc::new(move |_: &CancellationToken| {
            calls.fetch_add(1, Ordering::SeqCst);
            Ok(ProcessSnapshot::from_processes(
                ["claude", "pi", "codex"]
                    .into_iter()
                    .enumerate()
                    .map(|(i, name)| ProcessMeta {
                        pid: 42 + i as i32,
                        name: name.into(),
                        alive: true,
                        cwd: cwd.clone(),
                        ..Default::default()
                    }),
            ))
        }),
        ..Default::default()
    }
}

#[test]
fn snapshots_share_one_process_capture_return_partial_results_and_never_write_state() {
    let temp = temp();
    let calls = Arc::new(AtomicUsize::new(0));
    let cfg = config(temp.path(), calls.clone());
    let root = cfg.cursor_path.parent().unwrap().parent().unwrap();
    write(
        &cfg.claude_sessions_dirs[0].join("live.json"),
        json!({"pid":42,"sessionId":"claude","cwd":root,"name":"A task","nameSource":"custom","status":"waiting","statusUpdatedAt":321,"entrypoint":"sdk-cli","startedAt":123}),
    );
    write(
        &cfg.pi_sessions_dir.join("project/live.jsonl"),
        json!({"type":"session","id":"pi","timestamp":"2026-01-01T00:00:00Z","cwd":root,"version":3}),
    );
    let codex = cfg.codex_sessions_dir.join("year/live.jsonl");
    write(
        &codex,
        json!({"type":"session_meta","payload":{"id":"codex","cwd":root,"timestamp":"2026-01-01T00:00:00Z","agent_nickname":"Scout"}}),
    );
    fs::write(&cfg.opencode_db_path, "corrupt sqlite").unwrap();
    let scanner = Arc::new(Snapshotter::new(cfg.clone()));
    let cancel = CancellationToken::new();
    let result = scanner.snapshot(&cancel).unwrap();
    assert_eq!(calls.load(Ordering::SeqCst), 1);
    assert_eq!(
        result
            .sessions
            .iter()
            .map(|s| s.session_id.as_str())
            .collect::<Vec<_>>(),
        ["claude", "codex", "pi"]
    );
    assert_eq!(result.errors.len(), 1);
    assert_eq!(result.errors[0].harness, Harness::Opencode);
    assert!(!result.errors[0].error.to_string().is_empty());
    let claude = &result.sessions[0];
    assert_eq!(claude.name, "A task");
    assert_eq!(claude.state, SessionState::Waiting);
    assert_eq!(claude.state_updated_at, 321);
    assert_eq!(claude.entrypoint, "sdk-cli");
    assert!(!claude.subagent);
    assert_eq!(result.sessions[1].name, "Scout");
    assert_eq!(result.sessions[2].state, SessionState::Unknown);
    let serialized = serde_json::to_value(claude).unwrap();
    assert_eq!(serialized["state"], "waiting");
    assert_eq!(serialized["stateUpdatedAt"], 321);
    assert!(serialized.get("transcriptPath").is_none());
    for (status, state) in [
        ("busy", SessionState::Working),
        ("idle", SessionState::Idle),
        ("shell", SessionState::Unknown),
    ] {
        write(
            &cfg.claude_sessions_dirs[0].join("live.json"),
            json!({"pid":42,"sessionId":"claude","cwd":root,"status":status}),
        );
        assert_eq!(scanner.snapshot(&cancel).unwrap().sessions[0].state, state);
    }
    std::thread::scope(|scope| {
        for _ in 0..4 {
            scope.spawn(|| assert_eq!(scanner.snapshot(&cancel).unwrap().sessions.len(), 3));
        }
    });
    assert_eq!(calls.load(Ordering::SeqCst), 8);
    assert!(!cfg.cursor_path.parent().unwrap().exists());
    cancel.cancel();
    assert_eq!(
        scanner.snapshot(&cancel).unwrap_err().kind(),
        io::ErrorKind::Interrupted
    );
    assert_eq!(calls.load(Ordering::SeqCst), 8);
}

#[test]
fn cached_codex_state_refreshes_after_appends_and_process_resume() {
    use std::io::Write;
    let temp = temp();
    let mut cfg = config(temp.path(), Arc::new(AtomicUsize::new(0)));
    cfg.harnesses = vec![Harness::Codex];
    let root = cfg.codex_sessions_dir.parent().unwrap().to_owned();
    let path = cfg.codex_sessions_dir.join("year/session.jsonl");
    write(
        &path,
        json!({"type":"session_meta","payload":{"id":"codex","cwd":root,"timestamp":"2026-01-01T00:00:00Z"}}),
    );
    let scanner = Snapshotter::new(cfg.clone());
    let cancel = CancellationToken::new();
    assert_eq!(
        scanner.snapshot(&cancel).unwrap().sessions[0].state,
        SessionState::Unknown
    );
    let mut file = fs::OpenOptions::new().append(true).open(&path).unwrap();
    for (kind, state) in [
        ("task_started", SessionState::Working),
        ("turn_complete", SessionState::Idle),
        ("turn_started", SessionState::Working),
        ("turn_aborted", SessionState::Idle),
    ] {
        writeln!(
            file,
            "{}",
            json!({"type":"event_msg","timestamp":"2026-01-02T00:00:00Z","payload":{"type":kind}})
        )
        .unwrap();
        // A newer non-boundary containing the same word must not mask the boundary.
        writeln!(
            file,
            "{}",
            json!({"type":"response_item","payload":{"text":"task_started"}})
        )
        .unwrap();
        let first = scanner.snapshot(&cancel).unwrap();
        assert_eq!(first.sessions[0].state, state);
        assert_eq!(first.sessions[0].state_updated_at, 1767312000000);
        assert_eq!(scanner.snapshot(&cancel).unwrap().sessions, first.sessions);
    }
    writeln!(file, "{}", json!({"type":"event_msg","timestamp":"2026-01-02T00:00:00Z","payload":{"type":"task_started"}})).unwrap();
    cfg.process_snapshot_provider = Arc::new(move |_: &CancellationToken| {
        Ok(ProcessSnapshot::from_processes([ProcessMeta {
            pid: 44,
            name: "codex".into(),
            alive: true,
            cwd: root.clone(),
            create_time: 1767398400000,
            ..Default::default()
        }]))
    });
    assert_eq!(
        Snapshotter::new(cfg).snapshot(&cancel).unwrap().sessions[0].state,
        SessionState::Idle
    );
}

#[test]
fn process_snapshot_failure_is_returned_without_scanning_or_creating_storage() {
    let temp = temp();
    let mut cfg = config(temp.path(), Arc::new(AtomicUsize::new(0)));
    cfg.harnesses.clear();
    cfg.process_snapshot_provider =
        Arc::new(|_: &CancellationToken| Err(io::Error::other("process capture failed")));
    assert_eq!(
        Snapshotter::new(cfg)
            .snapshot(&CancellationToken::new())
            .unwrap_err()
            .to_string(),
        "process capture failed"
    );
    assert_eq!(fs::read_dir(temp.path()).unwrap().count(), 0);
}
