//! Cached, read-only live discovery. No cursors, checkpoint files or workers are created.
use crate::{Config, Harness, discovery::Discovery, scanner::SessionState};
use serde::{Deserialize, Serialize};
use std::{io, path::PathBuf, sync::Mutex};
use tokio_util::sync::CancellationToken;

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct Session {
    pub harness: Harness,
    pub harness_version: String,
    pub session_id: String,
    pub pid: i32,
    pub cwd: PathBuf,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub name: String,
    pub started_at: i64,
    pub last_activity: i64,
    pub subagent: bool,
    #[serde(skip_serializing_if = "empty_path")]
    pub transcript_path: PathBuf,
    pub state: SessionState,
    #[serde(skip_serializing_if = "is_zero")]
    pub state_updated_at: i64,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub entrypoint: String,
}
fn is_zero(value: &i64) -> bool {
    *value == 0
}
fn empty_path(value: &std::path::Path) -> bool {
    value.as_os_str().is_empty()
}

impl crate::watcher::Session {
    pub fn snapshot(&self) -> Session {
        Session {
            harness: self.event.harness,
            harness_version: self.event.harness_version.clone(),
            session_id: self.event.session_id.clone(),
            pid: self.event.pid,
            cwd: self.event.parsed.cwd.clone().into(),
            name: self.name.clone(),
            started_at: self.started_at,
            last_activity: if self.last_active != 0 {
                self.last_active
            } else {
                self.event.parsed.timestamp
            },
            subagent: self.event.subagent_id.is_some(),
            transcript_path: self.path.clone(),
            state: self.state,
            state_updated_at: self.state_updated_at,
            entrypoint: self.entrypoint.clone(),
        }
    }
}

#[derive(Debug)]
pub struct ScanFailure {
    pub harness: Harness,
    pub error: io::Error,
}
/// A failed harness does not hide sessions discovered from the other harnesses.
#[derive(Debug, Default)]
pub struct Snapshot {
    pub sessions: Vec<Session>,
    pub errors: Vec<ScanFailure>,
}

/// Reuses scanner caches across calls, including concurrent callers.
pub struct Snapshotter {
    config: Config,
    discovery: Mutex<Discovery>,
}
impl Snapshotter {
    pub fn new(mut config: Config) -> Self {
        if config.harnesses.is_empty() {
            config.harnesses = Config::default().harnesses;
        }
        Self {
            discovery: Mutex::new(Discovery::new(&config)),
            config,
        }
    }
    pub fn snapshot(&self, cancel: &CancellationToken) -> io::Result<Snapshot> {
        if cancel.is_cancelled() {
            return Err(io::Error::new(
                io::ErrorKind::Interrupted,
                "session scan cancelled",
            ));
        }
        let mut discovery = self
            .discovery
            .lock()
            .map_err(|error| io::Error::other(error.to_string()))?;
        let processes = self.config.process_snapshot_provider.snapshot(cancel)?;
        let mut result = Snapshot::default();
        for &harness in &self.config.harnesses {
            if cancel.is_cancelled() {
                return Err(io::Error::new(
                    io::ErrorKind::Interrupted,
                    "session scan cancelled",
                ));
            }
            match discovery.discover(cancel, harness, &processes) {
                Ok(sessions) => result.sessions.extend(
                    sessions
                        .iter()
                        .filter(|session| session.active)
                        .map(crate::watcher::Session::snapshot),
                ),
                Err(error) => result.errors.push(ScanFailure { harness, error }),
            }
        }
        result.sessions.sort_by(|a, b| {
            (a.harness.to_string(), &a.session_id).cmp(&(b.harness.to_string(), &b.session_id))
        });
        Ok(result)
    }
}
