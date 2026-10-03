package scanner

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestOpencodeChildrenShareParentProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.db")
	createTestOpencodeDB(t, path)
	db, err := sql.Open("sqlite3", "file:"+path+"?mode=rwc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, row := range []struct {
		id, parent string
		archived   any
	}{
		{"root", "", nil}, {"child", "root", nil}, {"grandchild", "child", nil},
		{"archived", "root", 2000}, {"archived-child", "archived", nil},
		{"orphan", "missing", nil}, {"cycle-a", "cycle-b", nil}, {"cycle-b", "cycle-a", nil},
	} {
		var parent any
		if row.parent != "" {
			parent = row.parent
		}
		_, err := db.Exec(`INSERT INTO session (id, parent_id, directory, title, version, tokens_input, tokens_output, tokens_reasoning, tokens_cache_read, tokens_cache_write, time_created, time_updated, time_archived) VALUES (?, ?, '/workspace', '', '1.0', 0, 0, 0, 0, 0, 1000, ?, ?)`, row.id, parent, 2000+len(row.id), row.archived)
		if err != nil {
			t.Fatal(err)
		}
	}
	s := NewOpencodeScanner(path, func(context.Context) ([]OpencodeProcessInfo, error) {
		return []OpencodeProcessInfo{{PID: 1234, CWD: "/workspace", CreateTime: 950}}, nil
	})
	sessions, err := s.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 8 {
		t.Fatalf("sessions = %d", len(sessions))
	}
	for _, session := range sessions {
		wantActive := session.SessionID == "root" || session.SessionID == "child" || session.SessionID == "grandchild"
		if session.IsActive != wantActive || wantActive && session.PID != 1234 {
			t.Errorf("session %s: active=%t PID=%d, want active=%t", session.SessionID, session.IsActive, session.PID, wantActive)
		}
	}
}
