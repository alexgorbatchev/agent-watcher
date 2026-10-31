package scanner

// SessionState says what a live agent is doing. Harnesses that record nothing
// comparable report SessionStateUnknown, which still means the session is running.
type SessionState string

const (
	SessionStateUnknown SessionState = "unknown"
	SessionStateWorking SessionState = "working"
	SessionStateWaiting SessionState = "waiting"
	SessionStateIdle    SessionState = "idle"
)

// ClaudeSessionState maps the session file's status. The field is undocumented,
// so anything else is unknown, including "shell", whose meaning is not established.
func ClaudeSessionState(status string) SessionState {
	switch status {
	case "busy":
		return SessionStateWorking
	case "waiting":
		return SessionStateWaiting
	case "idle":
		return SessionStateIdle
	default:
		return SessionStateUnknown
	}
}
