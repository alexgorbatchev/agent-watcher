use serde::{Deserialize, Serialize};

/// A live session may be running without a harness reporting its current state.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum SessionState {
    #[default]
    Unknown,
    Working,
    Waiting,
    Idle,
}

pub fn claude_session_state(status: &str) -> SessionState {
    match status {
        "busy" => SessionState::Working,
        "waiting" => SessionState::Waiting,
        "idle" => SessionState::Idle,
        _ => SessionState::Unknown,
    }
}
