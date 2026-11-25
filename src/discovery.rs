//! Lifecycle timestamp selection shared by harness discovery.
pub fn start_timestamp(started_at: i64, last_active: i64, now: i64) -> i64 {
    if last_active > 0 && last_active < now {
        last_active
    } else if started_at > 0 {
        started_at
    } else {
        now
    }
}

use crate::{
    Config, SessionSnapshot,
    scanner::*,
    watcher::{Event, Harness, Runner, Session},
};
use agent_parser::{EventData, ParsedEvent, claude_code_activity_timestamp};
use std::{
    collections::{BTreeMap, HashSet},
    io,
};
use tokio_util::sync::CancellationToken;
pub(crate) struct Discovery {
    claude: ClaudeCodeScanner,
    pi: PiScanner,
    codex: CodexScanner,
    opencode: OpencodeScanner,
    agy: AgyScanner,
}

impl Discovery {
    pub fn new(config: &Config) -> Self {
        let mut claude = ClaudeCodeScanner::new(config.claude_sessions_dirs.clone());
        if !config.claude_projects_dir.as_os_str().is_empty() {
            claude.set_projects_dir(&config.claude_projects_dir);
        }
        Self {
            claude,
            pi: PiScanner::new(config.pi_sessions_dir.clone()),
            codex: CodexScanner::new(config.codex_sessions_dir.clone()),
            opencode: OpencodeScanner::new(config.opencode_db_path.clone()),
            agy: AgyScanner::new(config.agy_sessions_dir.clone()),
        }
    }
}

impl Runner {
    pub async fn scan(&mut self, cancel: &CancellationToken) {
        let snapshot = match self.cfg.process_snapshot_provider.snapshot(cancel) {
            Ok(snapshot) => snapshot,
            Err(error) => {
                tracing::warn!(action="process_snapshot_failed",%error);
                return;
            }
        };
        for harness in self.cfg.harnesses.clone() {
            if cancel.is_cancelled() {
                break;
            }
            match self.discovery.discover(cancel, harness, &snapshot) {
                Ok(sessions) => self.reconcile(cancel, harness, sessions).await,
                Err(error) => tracing::warn!(action="session_scan_failed",%harness,%error),
            }
        }
        if let Err(error) = self.tracker.save() {
            tracing::warn!(action="session_tracker_save_failed",%error);
        }
    }
}
impl Discovery {
    pub fn discover(
        &mut self,
        cancel: &CancellationToken,
        harness: Harness,
        snapshot: &ProcessSnapshot,
    ) -> io::Result<Vec<Session>> {
        let descriptors = match harness {
            Harness::ClaudeCode => self.claude.scan(cancel, snapshot)?,
            Harness::Pi => self.pi.scan(cancel, snapshot)?,
            Harness::Codex => self.codex.scan(cancel, snapshot)?,
            Harness::Opencode => self.opencode.scan(cancel, snapshot)?,
            Harness::Agy => self.agy.scan(cancel, snapshot)?,
        };
        let now = chrono::Utc::now().timestamp_millis();
        descriptors
            .into_iter()
            .map(|descriptor| {
                if cancel.is_cancelled() {
                    return Err(io::Error::new(
                        io::ErrorKind::Interrupted,
                        "session scan cancelled",
                    ));
                }
                let mut last = descriptor.last_active;
                let mut data = EventData::default();
                if harness == Harness::ClaudeCode {
                    last = last_native_activity(
                        cancel,
                        &descriptor.transcript_path,
                        claude_code_activity_timestamp,
                    )
                    .unwrap_or(0);
                    if cancel.is_cancelled() {
                        return Err(io::Error::new(
                            io::ErrorKind::Interrupted,
                            "session scan cancelled",
                        ));
                    }
                    if descriptor.name_source == "custom" && !descriptor.name.is_empty() {
                        data.title = Some(descriptor.name.clone());
                    }
                }
                if harness == Harness::Opencode {
                    last = 0;
                }
                if matches!(harness, Harness::Opencode | Harness::Agy)
                    && !descriptor.title.is_empty()
                {
                    data.title = Some(descriptor.title.clone());
                }
                let subagent_id = (descriptor.is_subagent
                    && ((harness == Harness::Codex && !descriptor.parent_thread_id.is_empty())
                        || (matches!(harness, Harness::Opencode | Harness::Agy)
                            && !descriptor.parent_id.is_empty())))
                .then(|| descriptor.session_id.clone());
                let timestamp = start_timestamp(descriptor.started_at, last, now);
                let last_activity = if harness == Harness::Opencode {
                    descriptor.updated_at
                } else {
                    timestamp
                };
                let name = match harness {
                    Harness::ClaudeCode => descriptor.name.clone(),
                    Harness::Codex => descriptor.agent_nickname.clone(),
                    _ => descriptor.title.clone(),
                };
                let snapshot = SessionSnapshot {
                    harness,
                    harness_version: descriptor.harness_version.clone(),
                    session_id: descriptor.session_id.clone(),
                    pid: descriptor.pid,
                    cwd: descriptor.cwd.clone(),
                    name,
                    started_at: descriptor.started_at,
                    last_activity: if last_activity == 0 {
                        timestamp
                    } else {
                        last_activity
                    },
                    subagent: subagent_id.is_some(),
                    transcript_path: descriptor.transcript_path.clone(),
                    state: descriptor.state,
                    state_updated_at: descriptor.state_updated_at,
                    entrypoint: descriptor.entrypoint.clone(),
                };
                Ok(Session {
                    event: Event {
                        parsed: ParsedEvent {
                            timestamp,
                            cwd: snapshot.cwd.to_string_lossy().into(),
                            data,
                            ..Default::default()
                        },
                        harness,
                        harness_version: snapshot.harness_version.clone(),
                        session_id: snapshot.session_id.clone(),
                        subagent_id,
                        pid: descriptor.pid,
                    },
                    active: descriptor.is_active,
                    snapshot,
                })
            })
            .collect()
    }
}
impl Runner {
    async fn reconcile(
        &mut self,
        cancel: &CancellationToken,
        harness: Harness,
        sessions: Vec<Session>,
    ) {
        let previous = self.active.remove(&harness).unwrap_or_default();
        let mut next = BTreeMap::new();
        let mut filtered = HashSet::new();
        for session in sessions {
            if !session.active {
                continue;
            }
            let id = session.event.session_id.clone();
            if self
                .cfg
                .session_filter
                .as_ref()
                .is_some_and(|filter| !filter(&session.snapshot))
            {
                filtered.insert(id);
                continue;
            }
            if !previous.contains_key(&id)
                && let Err(error) = self
                    .lifecycle(cancel, session.event.clone(), "run_started")
                    .await
            {
                tracing::warn!(action="lifecycle_emit_failed",%error);
                continue;
            }
            self.tracker.set(TrackedSession {
                session_id: id.clone(),
                harness: harness.to_string(),
                pid: session.event.pid,
                cwd: session.event.parsed.cwd.clone().into(),
                harness_version: session.event.harness_version.clone(),
                started_at: session.snapshot.started_at,
                is_active: true,
            });
            self.attach(
                cancel,
                &session.snapshot.transcript_path,
                session.event.clone(),
            );
            self.attach_subagents(cancel, &session).await;
            if harness == Harness::Opencode {
                let result = if self.initial_scan && self.cfg.start_at_end {
                    self.baseline_opencode(cancel, &session).await
                } else {
                    self.poll(cancel, &session).await
                };
                if let Err(error) = result {
                    tracing::warn!(action="database_poll_failed",%error);
                }
            }
            next.insert(id, session);
        }
        for (id, old) in previous {
            if next.contains_key(&id) {
                continue;
            }
            // Drain both inactive descriptors and vanished sessions before exit.
            if harness == Harness::Opencode
                && !filtered.contains(&id)
                && let Err(error) = self.poll(cancel, &old).await
                && error.kind() != io::ErrorKind::NotFound
            {
                tracing::warn!(action="database_final_drain_failed",%error);
                next.insert(id, old);
                continue;
            }
            if harness == Harness::Opencode {
                match self.checkpoints.has_pending_session(&id) {
                    Ok(false) => {}
                    Ok(true) => {
                        next.insert(id, old);
                        continue;
                    }
                    Err(error) => {
                        tracing::warn!(action="database_pending_read_failed",%error);
                        next.insert(id, old);
                        continue;
                    }
                }
            }
            let mut event = old.event.clone();
            event.parsed.timestamp = chrono::Utc::now().timestamp_millis();
            event.parsed.data = EventData::default();
            if let Err(error) = self.lifecycle(cancel, event, "run_exited").await {
                tracing::warn!(action="lifecycle_emit_failed",%error);
                next.insert(id, old);
                continue;
            }
            self.detach(harness, &id);
            self.tracker.delete(&harness.to_string(), &id);
        }
        self.active.insert(harness, next);
    }
}
