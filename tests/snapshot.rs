use agent_watcher::{
    Config, Harness, Snapshotter,
    scanner::{ProcessMeta, ProcessSnapshot, SessionState},
    snapshot,
};
use serde_json::json;
use std::{
    fs::{self, File, OpenOptions},
    io::{self, Write},
    path::Path,
    sync::{
        Arc,
        atomic::{AtomicUsize, Ordering},
    },
    time::{Duration, UNIX_EPOCH},
};
use tokio_util::sync::CancellationToken;

const START: i64 = 1_700_000_000_000;
const START_TEXT: &str = "2023-11-14T22:13:20Z";
const WORKING_TEXT: &str = "2023-11-14T22:13:30Z";

fn write(path: &Path, content: &str) {
    fs::create_dir_all(path.parent().unwrap()).unwrap();
    fs::write(path, content).unwrap();
}

fn process(pid: i32, name: &str, cwd: &Path) -> ProcessMeta {
    ProcessMeta {
        pid,
        name: name.into(),
        create_time: START,
        cwd: cwd.to_owned(),
        alive: true,
        ..Default::default()
    }
}

#[test]
fn snapshots_share_process_capture_keep_harness_errors_and_refresh_codex_state() {
    let dir = tempfile::tempdir().unwrap();
    let root = dir.path();
    let capture_count = Arc::new(AtomicUsize::new(0));
    let processes = ProcessSnapshot::from_processes_with_parents(
        [
            process(41, "codex", root),
            process(42, "codex-tui", root),
            process(43, "claude", root),
        ],
        [(42, 41)],
    );
    let config = Config {
        harnesses: vec![Harness::Opencode, Harness::Codex, Harness::ClaudeCode],
        claude_sessions_dirs: vec![root.join("claude/sessions")],
        codex_sessions_dir: root.join("codex"),
        opencode_db_path: root.join("bad.db"),
        cursor_path: root.join("forbidden-cursor"),
        session_state_path: root.join("forbidden-state"),
        process_snapshot_provider: Arc::new({
            let capture_count = capture_count.clone();
            move |_: &CancellationToken| {
                capture_count.fetch_add(1, Ordering::SeqCst);
                Ok(processes.clone())
            }
        }),
        ..Default::default()
    };
    write(&config.opencode_db_path, "invalid SQLite");
    let transcript = root.join("claude/session.jsonl");
    write(
        &config.claude_sessions_dirs[0].join("43.json"),
        &json!({
            "pid":43, "sessionId":"claude", "cwd":root, "name":"Headless",
            "status":"waiting", "statusUpdatedAt":START + 4, "entrypoint":"sdk-cli",
            "transcriptPath":transcript, "startedAt":START
        })
        .to_string(),
    );
    write(
        &transcript,
        &format!(
            "{}\n{}\n",
            json!({"type":"user","timestamp":START_TEXT}),
            json!({"type":"last-prompt","timestamp":WORKING_TEXT}),
        ),
    );
    write(&root.join("claude/claude/subagents/retained.jsonl"), "{}\n");
    let rollout = |id: &str, parent: &str, recent: bool| {
        let path = config.codex_sessions_dir.join(format!("{id}.jsonl"));
        let header = json!({"type":"session_meta","timestamp":START_TEXT,
            "payload":{"id":id, "cwd":root, "timestamp":START_TEXT,
                "parent_thread_id":parent, "cli_version":"1", "agent_nickname":id}});
        let turn = json!({"type":"event_msg","timestamp":WORKING_TEXT,
            "payload":{"type":"task_started"}});
        write(&path, &format!("{header}\n{turn}\n"));
        if !recent {
            fs::write(&path, format!("{header}\n")).unwrap();
            File::open(&path)
                .unwrap()
                .set_modified(UNIX_EPOCH + Duration::from_millis(START as u64))
                .unwrap();
        }
        path
    };
    let parent = rollout("parent", "", true);
    rollout("historical", "", false);
    rollout("child", "parent", true);
    rollout("grandchild", "child", true);
    rollout("cycle_a", "cycle_b", true);
    rollout("cycle_b", "cycle_a", true);

    let cancel = CancellationToken::new();
    let snapshotter = Snapshotter::new(config.clone());
    let first = snapshotter.snapshot(&cancel);
    assert_eq!(capture_count.load(Ordering::SeqCst), 1);
    assert_eq!(first.errors.len(), 1);
    assert_eq!(first.errors[0].harness, Some(Harness::Opencode));
    assert_eq!(
        first
            .sessions
            .iter()
            .map(|s| s.session_id.as_str())
            .collect::<Vec<_>>(),
        ["claude", "child", "grandchild", "parent"]
    );
    let claude = &first.sessions[0];
    assert_eq!(claude.state, SessionState::Waiting);
    assert_eq!(claude.state_updated_at, START + 4);
    assert_eq!(claude.entrypoint, "sdk-cli");
    assert_eq!(claude.last_activity, START);
    assert_eq!(
        serde_json::to_value(claude).unwrap()["entrypoint"],
        "sdk-cli"
    );
    for session in &first.sessions[1..] {
        assert_eq!(session.pid, 41);
        assert_eq!(session.state, SessionState::Working);
        assert_eq!(session.state_updated_at, START + 10_000);
        assert_eq!(session.subagent, session.session_id != "parent");
    }
    writeln!(&mut OpenOptions::new().append(true).open(&parent).unwrap(), "{}",
        json!({"type":"event_msg","timestamp":"2023-11-14T22:13:32Z","payload":{"type":"turn_aborted"}})
    ).unwrap();
    let refreshed = snapshotter.snapshot(&cancel);
    assert_eq!(capture_count.load(Ordering::SeqCst), 2);
    assert_eq!(refreshed.sessions[3].state, SessionState::Idle);
    assert_eq!(refreshed.sessions[3].state_updated_at, START + 12_000);
    assert!(!config.cursor_path.exists() && !config.session_state_path.exists());
    cancel.cancel();
    let failed = snapshot(&cancel, config);
    assert_eq!(capture_count.load(Ordering::SeqCst), 2);
    assert!(failed.sessions.is_empty());
    assert_eq!(failed.errors[0].error.kind(), io::ErrorKind::Interrupted);
}

#[test]
fn agy_uses_summary_state_local_child_links_and_the_full_native_transcript() {
    let dir = tempfile::tempdir().unwrap();
    let root = dir.path();
    let brain = root.join("brain");
    let parent = brain.join("parent/.system_generated");
    let child = brain.join("child/.system_generated");
    let full = parent.join("logs/transcript_full.jsonl");
    write(
        &full,
        &format!(
            "{}\n{}\n",
            json!({"type":"USER_INPUT","created_at":WORKING_TEXT}),
            json!({"type":"SYSTEM","created_at":"2026-01-01T00:00:00Z"}),
        ),
    );
    write(&parent.join("logs/transcript.jsonl"), "{}\n");
    write(&child.join("logs/transcript.jsonl"), "{}\n");
    write(&parent.join("subagents/child.json"), "{}");
    let db = rusqlite::Connection::open(root.join("conversation_summaries.db")).unwrap();
    db.execute_batch("CREATE TABLE conversation_summaries (conversation_id TEXT, title TEXT, workspace_uris TEXT, status TEXT, parent_conversation_id TEXT, last_modified_time TEXT, not_fully_idle, killed);").unwrap();
    db.execute(
        "INSERT INTO conversation_summaries VALUES ('parent','Work',?1,'done','',?2,1,0), ('child','Child','[]','WAITING','missing',?2,0,0)",
        rusqlite::params![json!([root]).to_string(), WORKING_TEXT],
    ).unwrap();
    let cwd = root.to_owned();
    let config = Config {
        harnesses: vec![Harness::Agy],
        agy_sessions_dir: brain,
        process_snapshot_provider: Arc::new(move |_: &CancellationToken| {
            Ok(ProcessSnapshot::from_processes([process(42, "agy", &cwd)]))
        }),
        ..Default::default()
    };
    let snap = snapshot(&CancellationToken::new(), config);
    assert!(snap.errors.is_empty(), "{:?}", snap.errors);
    assert_eq!(snap.sessions.len(), 2);
    let child = &snap.sessions[0];
    assert!(child.subagent);
    assert_eq!(child.pid, 42);
    assert_eq!(child.state, SessionState::Waiting);
    let parent = &snap.sessions[1];
    assert_eq!(parent.transcript_path, full);
    assert_eq!(parent.started_at, START);
    assert_eq!(parent.name, "Work");
    assert_eq!(parent.state, SessionState::Working);
    assert_eq!(parent.state_updated_at, START + 10_000);
    assert_eq!(parent.last_activity, START + 10_000);
    let wire = serde_json::to_value(parent).unwrap();
    assert_eq!(wire["state"], "working");
    assert!(wire.get("entrypoint").is_none());
}
