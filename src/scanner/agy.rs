use super::{
    ProcessSnapshot, SessionDescriptor, SessionState, check_cancel, default_agy_sessions_dir,
    paths::clean_path,
    timestamps::{last_native_activity, mtime},
    transcripts::FileStamp,
};
use agent_parser::{agy_activity_timestamp, read_agy_conversation_workspaces, read_agy_summaries};
use std::{
    collections::HashMap,
    fs, io,
    path::{Path, PathBuf},
};
use tokio_util::sync::CancellationToken;

/// Discover live Antigravity CLI sessions and their detached subagents.
#[derive(Debug)]
pub struct AgyScanner {
    root: PathBuf,
    activity: HashMap<PathBuf, (FileStamp, i64)>,
}

/// Normalize the native conversation status.
pub fn agy_session_state(status: &str, not_fully_idle: bool) -> SessionState {
    match status.trim().to_ascii_uppercase().as_str() {
        "RUNNING" | "ACTIVE" => SessionState::Working,
        "WAITING" => SessionState::Waiting,
        _ if not_fully_idle => SessionState::Working,
        "IDLE" | "DONE" | "SUCCESS" => SessionState::Idle,
        _ => SessionState::Unknown,
    }
}

impl AgyScanner {
    pub fn new(root: PathBuf) -> Self {
        Self {
            root: if root.as_os_str().is_empty() {
                default_agy_sessions_dir()
            } else {
                root
            },
            activity: HashMap::new(),
        }
    }

    pub fn scan(
        &mut self,
        cancel: &CancellationToken,
        processes: &ProcessSnapshot,
    ) -> io::Result<Vec<SessionDescriptor>> {
        check_cancel(cancel)?;
        let root = match fs::canonicalize(&self.root) {
            Ok(root) => root,
            Err(error) if error.kind() == io::ErrorKind::NotFound => return Ok(Vec::new()),
            Err(error) => return Err(error),
        };
        let app_data = root.parent().unwrap_or(&root);
        let summaries =
            read_agy_summaries(app_data.join("conversation_summaries.db")).unwrap_or_default();
        let conversations = app_data.join("conversations");
        let mut parents = summaries
            .iter()
            .filter(|(_, summary)| !summary.parent_conversation_id.is_empty())
            .map(|(id, summary)| (id.clone(), summary.parent_conversation_id.clone()))
            .collect::<HashMap<_, _>>();
        let mut directories = Vec::new();
        for entry in fs::read_dir(&root)? {
            check_cancel(cancel)?;
            let entry = entry?;
            if entry.file_type()?.is_dir() {
                directories.push(entry.path());
            }
        }
        directories.sort_unstable();
        for directory in &directories {
            check_cancel(cancel)?;
            let id = directory.file_name().unwrap_or_default().to_string_lossy();
            let Ok(children) = fs::read_dir(directory.join(".system_generated/subagents")) else {
                continue;
            };
            for child in children.flatten() {
                check_cancel(cancel)?;
                let path = child.path();
                if path.extension().is_some_and(|ext| ext == "json")
                    && child.file_type().is_ok_and(|kind| !kind.is_dir())
                    && let Some(child_id) = path.file_stem()
                {
                    parents.insert(child_id.to_string_lossy().into_owned(), id.to_string());
                }
            }
        }

        let mut sessions = Vec::new();
        let mut activity = HashMap::new();
        for directory in directories {
            check_cancel(cancel)?;
            let id = directory.file_name().unwrap_or_default().to_string_lossy();
            let Some((path, metadata)) = transcript(&directory.join(".system_generated/logs"))
            else {
                continue;
            };
            let summary = summaries.get(id.as_ref());
            let cwd = summary
                .and_then(|summary| summary.workspace_paths.first().cloned())
                .or_else(|| {
                    read_agy_conversation_workspaces(&conversations, &id)
                        .ok()?
                        .into_iter()
                        .next()
                })
                .map_or_else(PathBuf::new, |cwd| clean_path(&cwd));
            let started_at = mtime(&metadata);
            let stamp = FileStamp::from(&metadata);
            let last_active = self
                .activity
                .get(&path)
                .filter(|(previous, _)| *previous == stamp)
                .map_or_else(
                    || {
                        last_native_activity(cancel, &path, agy_activity_timestamp)
                            .unwrap_or(started_at)
                    },
                    |(_, time)| *time,
                );
            check_cancel(cancel)?;
            activity.insert(path.clone(), (stamp, last_active));
            let parent_id = parents.remove(id.as_ref()).unwrap_or_default();
            let mut session = SessionDescriptor {
                session_id: id.into_owned(),
                cwd,
                transcript_path: path,
                started_at,
                last_active,
                is_subagent: !parent_id.is_empty(),
                parent_id,
                ..Default::default()
            };
            if let Some(summary) = summary {
                session.title.clone_from(&summary.title);
                session.state = agy_session_state(&summary.status, summary.not_fully_idle);
                session.state_updated_at = summary.last_modified.unwrap_or_default();
            }
            sessions.push(session);
        }
        self.activity = activity;
        sessions.sort_by(|a, b| {
            b.last_active
                .cmp(&a.last_active)
                .then_with(|| a.session_id.cmp(&b.session_id))
        });
        let mut active = HashMap::new();
        for process in processes.agy_processes() {
            if let Some(session) = sessions.iter_mut().find(|session| {
                !session.is_active && !session.is_subagent && session.cwd == process.cwd
            }) {
                session.pid = process.pid;
                session.is_active = true;
                if process.create_time > 0 {
                    session.started_at = process.create_time;
                }
                active.insert(session.session_id.clone(), process.pid);
            }
        }
        for session in &mut sessions {
            if let Some(&pid) = active.get(&session.parent_id) {
                session.pid = pid;
                session.is_active = true;
            }
        }
        Ok(sessions)
    }
}

fn transcript(directory: &Path) -> Option<(PathBuf, fs::Metadata)> {
    ["transcript_full.jsonl", "transcript.jsonl"]
        .into_iter()
        .find_map(|name| {
            let path = directory.join(name);
            let metadata = fs::metadata(&path).ok()?;
            metadata.is_file().then_some((path, metadata))
        })
}
