---
created_on: 2026-10-02 15:48
last_modified: 2026-10-02 21:21
status: current
---

# agent-watcher

Reusable Go discovery, lifecycle, transcript/database watching, and `agent-parser` event delivery. `agent-status` consumes the library and owns telemetry formatting, git workspace resolution, and WebSocket delivery.

## Commands

- Install dependencies: `go mod download`
- Test: `just test` (race detector and >=90% statement coverage per package)
- Lint: `just lint` (gofmt check, module hygiene, vet, golangci-lint)
- All checks: `just check`
- Format: `just fmt`
- Linter: `golangci-lint` v2.13.2, matching the pinned CI version.

## Conventions

- Read applicable skills before writing code; use Go 1.26.2+ conventions.
- `watcher.Run` owns workers and joins them on cancellation. Handlers may run concurrently and must respect their context.
- `scanner/` discovers sessions with one shared process snapshot per tick; `tailer/` owns filesystem watches and byte cursors. Keep event parsing in `agent-parser`.
- Transcript acknowledgement represents every event in one source line. Commit only a contiguous acknowledged prefix and bind callbacks to the file generation to reject acknowledgements from replaced files.
- Cursor pruning preserves existing transcripts with matching device and inode regardless of age; remove checkpoints for deleted or replaced files. Age retention applies to inaccessible files.
- OpenCode message watching is part of this library: use `agent-parser`'s read-only database API, emit changed message/part events, and checkpoint database revisions separately from transcript byte offsets. Drain final messages before emitting exit.
- OpenCode child sessions share their parent's process. Completed assistant usage is checkpointed once per message, including across restart.
- Logger injection uses `*slog.Logger`; messages are concise snake_case actions. Consumers choose output formatting.
- The inherited `AGENT_STATUS_TEST_MOCK_PROC` override is test-only. Prefer `Config.ProcessSnapshotProvider` in new tests.

## Gotchas

- Each concurrent watcher requires separate cursor, database checkpoint, and session-state paths; there is no inter-process checkpoint locking.
- Accepted but unacknowledged events remain pending in memory and replay on restart. Returning an error makes delivery retry; do not acknowledge a rejected event.
- Claude Code compaction can replace or truncate transcripts. Keep directory watches and file-generation checks intact.
- `Run` logs runtime read/persistence failures and retries where possible; initialization failures return errors. A handler that ignores cancellation can prevent shutdown.
- Consumers use tagged GitHub module versions. Keep local workspace overrides optional; consumers must build and test without a sibling checkout.

## Boundaries

- Always: automatically record new user instructions in the appropriate `AGENTS.md`, checking conflicts with the user.
- Always: functional code changes require behavioral test changes, red/green verification, and >=90% statement coverage per package. Temporarily disable the change and verify its regression test fails before completing work.
- Always: keep the full watching pipeline reusable; do not import `agent-status` or add application telemetry/transport contracts.
- Always: publish the extracted library through GitHub and integrate tagged versions into `agent-status`; initial publication is authorized by the user, and future releases require explicit authorization.
- Always: use `rg` or codegraph; keep scratch files under `.tmp`; never use heredocs.
- Ask first: changing existing transcript cursor storage or application wire protocols.
- Never: write to, lock, mutate, or delete harness logs or databases; parse or transmit credentials; publish releases without explicit authorization.
- Never: modify or stage unrelated concurrent changes.

## References

- Public entry point and contract: `watcher.go`
- Discovery and lifecycle: `discovery.go`, `scanner/`
- Transcript delivery and checkpoints: `transcript.go`, `delivery.go`, `tailer/`
- Database delivery and checkpoints: `opencode.go`, `checkpoint.go`
- Parser and canonical harness schemas: `github.com/alexgorbatchev/agent-parser`
- Consumer integration: `github.com/alexgorbatchev/agent-status`, `packages/agent-observer/cmd/air-observer/main.go`
