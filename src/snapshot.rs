use crate::{Config, Harness, discovery::Discovery, scanner::SessionState};
use serde::{Deserialize, Serialize};
use std::{
    io,
    path::{Path, PathBuf},
    sync::Mutex,
};
use tokio_util::sync::CancellationToken;

/// The current state of one running agent session. Times are Unix milliseconds.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "camelCase")]
pub struct SessionSnapshot {
    pub harness: Harness,
    pub harness_version: String,
    pub session_id: String,
    pub pid: i32,
    pub cwd: PathBuf,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub name: String,
    pub started_at: i64,
    pub last_activity: i64,
    pub subagent: bool,
    #[serde(default, skip_serializing_if = "empty_path")]
    pub transcript_path: PathBuf,
    pub state: SessionState,
    #[serde(default, skip_serializing_if = "zero")]
    pub state_updated_at: i64,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub entrypoint: String,
}

fn empty_path(path: &Path) -> bool {
    path.as_os_str().is_empty()
}

fn zero(value: &i64) -> bool {
    *value == 0
}

#[derive(Debug)]
pub struct SnapshotError {
    /// Process capture and cancellation errors have no associated harness.
    pub harness: Option<Harness>,
    pub error: io::Error,
}

/// Successful scans are returned alongside failures from individual harnesses.
#[derive(Debug, Default)]
pub struct SnapshotResult {
    pub sessions: Vec<SessionSnapshot>,
    pub errors: Vec<SnapshotError>,
}

/// Reuse discovery caches across snapshots. Discovery never writes watcher state.
pub struct Snapshotter {
    config: Config,
    discovery: Mutex<Discovery>,
}

impl Snapshotter {
    pub fn new(mut config: Config) -> Self {
        config.apply_discovery_defaults();
        Self {
            discovery: Mutex::new(Discovery::new(&config)),
            config,
        }
    }

    pub fn snapshot(&self, cancel: &CancellationToken) -> SnapshotResult {
        let mut result = SnapshotResult::default();
        let mut discovery = self.discovery.lock().unwrap();
        let processes = if cancel.is_cancelled() {
            Err(io::Error::new(
                io::ErrorKind::Interrupted,
                "snapshot cancelled",
            ))
        } else {
            self.config.process_snapshot_provider.snapshot(cancel)
        };
        let processes = match processes {
            Ok(processes) => processes,
            Err(error) => {
                result.errors.push(SnapshotError {
                    harness: None,
                    error,
                });
                return result;
            }
        };
        for &harness in &self.config.harnesses {
            if cancel.is_cancelled() {
                result.errors.push(SnapshotError {
                    harness: None,
                    error: io::Error::new(io::ErrorKind::Interrupted, "snapshot cancelled"),
                });
                break;
            }
            match discovery.discover(cancel, harness, &processes) {
                Ok(sessions) => result.sessions.extend(
                    sessions
                        .into_iter()
                        .filter(|session| session.active)
                        .map(|session| session.snapshot),
                ),
                Err(error) => result.errors.push(SnapshotError {
                    harness: Some(harness),
                    error,
                }),
            }
        }
        result.sessions.sort_by(|a, b| {
            a.harness
                .as_str()
                .cmp(b.harness.as_str())
                .then_with(|| a.session_id.cmp(&b.session_id))
        });
        result
    }
}

/// List currently live sessions without starting a watcher.
pub fn snapshot(cancel: &CancellationToken, config: Config) -> SnapshotResult {
    Snapshotter::new(config).snapshot(cancel)
}
