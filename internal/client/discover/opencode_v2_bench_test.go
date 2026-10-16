package discover

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// BenchmarkDiscoverOpenCodeV2Unchanged measures complete warmed discovery passes,
// including database reads and unchanged cache comparisons. The corpus is wholly
// synthetic; no live session or credential tables are read. Keep different session
// counts so per-session overhead remains visible alongside transcript byte cost.
func BenchmarkDiscoverOpenCodeV2Unchanged(b *testing.B) {
	for _, shape := range []struct {
		name     string
		sessions int
		bytes    int
	}{
		{"one-large-16MiB", 1, 16 << 20},
		{"many-small-16MiB", 256, 64 << 10},
		{"history-128MiB", 128, 1 << 20},
	} {
		b.Run(shape.name, func(b *testing.B) {
			dir, cache := b.TempDir(), b.TempDir()
			previous := openCodeCacheDir
			openCodeCacheDir = func(string) (string, error) { return cache, nil }
			b.Cleanup(func() { openCodeCacheDir = previous })
			db, err := sql.Open("sqlite", filepath.Join(dir, openCodeDBName))
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { db.Close() })
			_, err = db.Exec(`CREATE TABLE workspace (id TEXT PRIMARY KEY, branch TEXT);
CREATE TABLE session_v2 (id TEXT PRIMARY KEY, parent_id TEXT, workspace_id TEXT,
directory TEXT, title TEXT, slug TEXT, agent TEXT, model TEXT,
time_created INTEGER, time_updated INTEGER);
CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT, type TEXT,
seq INTEGER, time_created INTEGER, time_updated INTEGER, data TEXT);
CREATE INDEX session_message_order ON session_message(session_id, seq);`)
			if err != nil {
				b.Fatal(err)
			}
			data, err := json.Marshal(map[string]any{
				"agent": "build", "model": map[string]string{"providerID": "fixture", "id": "fixture"},
				"time":    map[string]int{"created": 1000, "completed": 2000},
				"content": []map[string]string{{"type": "text", "text": strings.Repeat("x", shape.bytes)}},
			})
			if err != nil {
				b.Fatal(err)
			}
			tx, err := db.Begin()
			if err != nil {
				b.Fatal(err)
			}
			for i := 0; i < shape.sessions; i++ {
				id := fmt.Sprintf("ses_%04d", i)
				if _, err := tx.Exec(`INSERT INTO session_v2 VALUES (?,NULL,NULL,'/fixture','fixture','fixture','build',NULL,1000,2000)`, id); err != nil {
					b.Fatal(err)
				}
				if _, err := tx.Exec(`INSERT INTO session_message VALUES (?,?, 'assistant',1,1000,2000,?)`, "msg_"+id, id, string(data)); err != nil {
					b.Fatal(err)
				}
			}
			if err := tx.Commit(); err != nil {
				b.Fatal(err)
			}
			roots := []Root{{Agent: "opencode", Dir: dir}}
			if files, _, err := Discover(roots, Excluder{}); err != nil || len(files) != shape.sessions {
				b.Fatalf("warm discovery: files=%d error=%v", len(files), err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(shape.sessions * shape.bytes))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if files, _, err := Discover(roots, Excluder{}); err != nil || len(files) != shape.sessions {
					b.Fatalf("discovery: files=%d error=%v", len(files), err)
				}
			}
		})
	}
}
