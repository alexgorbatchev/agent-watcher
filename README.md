`agent-watcher` discovers Claude Code, Pi, Codex, and OpenCode sessions and streams lifecycle and parsed activity events to Go applications. Consumers choose how to store, display, or transport events and acknowledge them after delivery.

# What It Does

- **Session discovery**: Matches harness sessions to running processes, including Claude Code transcripts and OpenCode child sessions.
- **Activity events**: Emits the event model from `agent-parser` together with harness, session, subagent, and process identity.
- **Incremental watching**: Follows JSONL transcripts through appends and atomic replacement, and polls OpenCode's read-only SQLite database for message and part revisions.
- **Acknowledged checkpoints**: Persists delivered transcript offsets and database event revisions for restart replay.

# How It Works

1. Call `watcher.Run` with a context, configuration, and event handler.
2. Receive `run_started`, parsed activity events, and `run_exited` as sessions appear, change, and exit.
3. Deliver each event. Call its acknowledgement when delivery is durable, if an acknowledgement is supplied.
4. Cancel the context and wait for `Run` to return so workers stop and checkpoints flush.

# How it Really Works

- `Run` blocks. Handlers for different transcripts can execute concurrently and must honor context cancellation. Each simultaneous `Run` needs separate checkpoint and session-state files.
- Discovery and OpenCode database reads run every two seconds by default. JSONL appends trigger filesystem watches. OpenCode completion usage is emitted only after a completion marker and once per acknowledged assistant message.
- A transcript line can produce several events. Only its last event has an acknowledgement, which represents delivery of the whole line. Each OpenCode event revision has its own acknowledgement. Lifecycle events have none.
- Returning a handler error pauses that transcript or retries the database event on a later scan. Accepted events are not re-emitted in the same run while their acknowledgement is pending. Unacknowledged events replay after restart. Duplicate delivery remains possible across crashes or checkpoint flushes, so consumers should tolerate replay.
- Default persistence lives in `$XDG_CACHE_HOME/agent-watcher/`, or `~/.cache/agent-watcher/`: `observer-cursors.json`, `observer-cursors.json.opencode`, and `observer-sessions.json`. Checkpoints flush every two seconds and on shutdown. Runtime read and persistence errors are logged; invalid configuration or unreadable initial checkpoint state makes `Run` return an error.
- Harness storage is read-only. The library performs no network delivery and does no work at import time. Empty configuration paths consult the harness environment variables listed below; configuration never changes process environment variables.

# Prerequisites

- [Go](https://go.dev/) 1.26.2 or newer.
- Local harness session files or an OpenCode SQLite database accessible to the current user, and permission to inspect that user's processes.

# Installation

```bash
go get github.com/alexgorbatchev/agent-watcher
```

# Quick Start

```go
package main

import (
    "context"
    "errors"
    "fmt"
    "os"
    "os/signal"

    watcher "github.com/alexgorbatchev/agent-watcher"
)

func main() {
    ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
    defer stop()
    err := watcher.Run(ctx, watcher.Config{}, func(ctx context.Context, event watcher.Event, acknowledge func()) error {
        if err := ctx.Err(); err != nil {
            return err
        }
        if _, err := fmt.Printf("%s %s %s\n", event.Harness, event.SessionID, event.EventType); err != nil {
            return err
        }
        if acknowledge != nil {
            acknowledge()
        }
        return nil
    })
    if err != nil && !errors.Is(err, context.Canceled) {
        fmt.Fprintln(os.Stderr, err)
        os.Exit(1)
    }
}
```

This handler acknowledges after writing to stdout. A durable consumer acknowledges after its storage transaction or downstream delivery succeeds.

# API

| Export | Purpose |
| :--- | :--- |
| `Run(ctx, config, handler) error` | Own discovery, watching, worker shutdown, and checkpoint persistence. Normal cancellation returns nil; an already cancelled context returns its error without I/O. |
| `Config` | Choose harnesses, paths, intervals, process discovery, and logger. |
| `Event` | Embed `agent-parser.ParsedEvent` and add `Harness`, `HarnessVersion`, `SessionID`, `SubagentID`, and `PID`. |
| `Handler` | Accept events and optional acknowledgements; return an error to apply backpressure. |
| `scanner` and `tailer` packages | Access lower-level discovery, process snapshots, cursor stores, and filesystem tailers. |

# Configuration

Explicit paths take precedence over environment-based defaults.

| Option | Default | Description |
| :--- | :--- | :--- |
| `Harnesses` | All four harnesses | Select `HarnessClaudeCode`, `HarnessPi`, `HarnessCodex`, or `HarnessOpencode`. |
| `ScanInterval` | `2s` | Discovery and database polling interval. |
| `ClaudeSessionsDirs` | Harness defaults | `$CLAUDE_SESSIONS_DIR`, ai-registry session storage under `$XDG_DATA_HOME` or `~/.local/share`, and `~/.claude/sessions`. |
| `ClaudeProjectsDir` | Harness defaults | Transcript lookup checks `$CLAUDE_PROJECTS_DIR`, ai-registry project storage, and `~/.claude/projects`. |
| `PiSessionsDir` | Harness defaults | `$PI_SESSIONS_DIR`, `$PI_CODING_AGENT_DIR/sessions`, or `~/.pi/agent/sessions`. |
| `CodexSessionsDir` | Harness defaults | `$CODEX_SESSIONS_DIR`, `$CODEX_HOME/sessions`, or `~/.codex/sessions`. |
| `OpencodeDBPath` | Harness defaults | `$OPENCODE_DB_PATH`, `$XDG_DATA_HOME/opencode/opencode.db`, or `~/.local/share/opencode/opencode.db`. |
| `CursorPath` | Cache path above | Acknowledged JSONL byte cursors. |
| `OpencodeCheckpointPath` | Resolved `CursorPath` + `.opencode` | Acknowledged OpenCode event revisions. |
| `SessionStatePath` | Cache path above | Last known running sessions for exit detection across restarts. |
| `CursorRetention` | `24h` | Retain cursors for inaccessible transcripts for this long. Existing transcripts with matching device and inode keep their cursors regardless of age; deleted or replaced files are pruned. |
| `CursorPruneInterval` | `1h` | Period between cursor pruning passes. |
| `CursorFlushInterval` | `2s` | Period between transcript and database checkpoint flushes. |
| `ProcessSnapshotProvider` | Live process discovery | Supply process snapshots for embedded applications or tests. |
| `Logger` | `slog.Default()` | Receive structured logs with concise snake_case messages. |

Nonpositive durations use their defaults. Missing harness storage produces no sessions. The inherited `AGENT_STATUS_TEST_MOCK_PROC=1` test override bypasses process verification; keep it unset in applications.

# License

[MIT](LICENSE), copyright 2026 Alex Gorbatchev.
