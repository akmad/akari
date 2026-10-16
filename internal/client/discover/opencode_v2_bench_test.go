package discover

import (
	"container/list"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

type openCodeV2BenchmarkShape struct {
	name            string
	sessions, bytes int
}

var openCodeV2BenchmarkShapes = []openCodeV2BenchmarkShape{
	{"one-large-16MiB", 1, 16 << 20},
	{"many-small-16MiB", 256, 64 << 10},
	{"history-128MiB", 128, 1 << 20},
}

func clearOpenCodeV2BenchmarkFingerprints() {
	cache := &openCodeV2Fingerprints
	cache.Lock()
	defer cache.Unlock()
	cache.entries = make(map[string]*list.Element)
	cache.order.Init()
}

// The benchmark mirrors the selected 2.0.30 workspace schema, not the old branch
// column. Fixture creation, cache reset and post-pass correctness checks are untimed.
func openCodeV2BenchmarkCorpus(b *testing.B, shape openCodeV2BenchmarkShape) ([]Root, string) {
	b.Helper()
	dir, cache := b.TempDir(), b.TempDir()
	previous := openCodeCacheDir
	openCodeCacheDir = func(string) (string, error) { return cache, nil }
	clearOpenCodeV2BenchmarkFingerprints()
	b.Cleanup(func() { openCodeCacheDir = previous; clearOpenCodeV2BenchmarkFingerprints() })
	db, err := sql.Open("sqlite", filepath.Join(dir, openCodeDBName))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE workspace (id TEXT PRIMARY KEY,provider TEXT NOT NULL,binding TEXT,created_at INTEGER NOT NULL,last_used_at INTEGER NOT NULL);
CREATE TABLE session_v2 (id TEXT PRIMARY KEY,parent_id TEXT,workspace_id TEXT,
directory TEXT,title TEXT,slug TEXT,agent TEXT,model TEXT,
time_created INTEGER,time_updated INTEGER);
CREATE TABLE session_message (id TEXT PRIMARY KEY,session_id TEXT,type TEXT,
seq INTEGER,time_created INTEGER,time_updated INTEGER,data TEXT);
CREATE INDEX session_message_order ON session_message(session_id,seq);`)
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
	defer tx.Rollback()
	for i := 0; i < shape.sessions; i++ {
		id := fmt.Sprintf("ses_%04d", i)
		if _, err := tx.Exec(`INSERT INTO session_v2 VALUES (?,NULL,NULL,'/fixture','fixture','fixture','build',NULL,1000,2000)`, id); err != nil {
			b.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO session_message VALUES (?,?,'assistant',1,1000,2000,?)`, "msg_"+id, id, string(data)); err != nil {
			b.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	roots := []Root{{Agent: "opencode", Dir: dir}}
	if files, _, err := Discover(roots, Excluder{}); err != nil || len(files) != shape.sessions {
		b.Fatalf("initial discovery: files=%d error=%v", len(files), err)
	}
	verifyOpenCodeV2BenchmarkCorpus(b, shape, cache)
	return roots, cache
}

func verifyOpenCodeV2BenchmarkCorpus(b *testing.B, shape openCodeV2BenchmarkShape, cacheDir string) {
	b.Helper()
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		b.Fatal(err)
	}
	if len(entries) != shape.sessions {
		b.Fatalf("materialized sessions=%d want %d", len(entries), shape.sessions)
	}
	for i := 0; i < shape.sessions; i++ {
		id := fmt.Sprintf("ses_%04d", i)
		raw, err := os.ReadFile(filepath.Join(cacheDir, id+".jsonl"))
		if err != nil {
			b.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		if len(lines) != 2 || gjson.Get(lines[0], "id").String() != id || gjson.Get(lines[1], "id").String() != "msg_"+id || gjson.Get(lines[1], "data.content.0.text").String() != strings.Repeat("x", shape.bytes) {
			b.Fatalf("session %s lost identity or body", id)
		}
	}
	openCodeV2Fingerprints.Lock()
	count := len(openCodeV2Fingerprints.entries)
	orderCount := openCodeV2Fingerprints.order.Len()
	openCodeV2Fingerprints.Unlock()
	want := min(shape.sessions, 4096)
	if count != want || orderCount != want {
		b.Fatalf("fingerprint entries/order=%d/%d want %d", count, orderCount, want)
	}
	// Sequential IDs are also discovery order. Verify the actual retained tail,
	// rather than treating the cache bound as a limit on discovered sessions.
	openCodeV2Fingerprints.Lock()
	for i := 0; i < shape.sessions; i++ {
		path := filepath.Join(cacheDir, fmt.Sprintf("ses_%04d.jsonl", i))
		retained := openCodeV2Fingerprints.entries[path] != nil
		if retained != (i >= shape.sessions-want) {
			openCodeV2Fingerprints.Unlock()
			b.Fatalf("unexpected fingerprint retention at session %d", i)
		}
	}
	openCodeV2Fingerprints.Unlock()
	b.ReportMetric(float64(shape.sessions-count), "outside-cache")
	b.ReportMetric(float64(count), "fingerprints")
	b.ReportMetric(float64(shape.sessions), "sessions")
}

// BenchmarkDiscoverOpenCodeV2Unchanged measures complete warmed passes. Every
// pass still hashes source rows and existing output; this is not an LRU lookup microbenchmark.
func BenchmarkDiscoverOpenCodeV2Unchanged(b *testing.B) {
	for _, shape := range openCodeV2BenchmarkShapes {
		b.Run(shape.name, func(b *testing.B) { benchmarkOpenCodeV2Discovery(b, shape, "") })
	}
}

// BenchmarkDiscoverOpenCodeV2Cold separates process startup with existing output
// from first materialization. Neither case flushes OS filesystem/page caches.
func BenchmarkDiscoverOpenCodeV2Cold(b *testing.B) {
	for _, shape := range []openCodeV2BenchmarkShape{openCodeV2BenchmarkShapes[1], openCodeV2BenchmarkShapes[2], {"over-capacity-4608", 4608, 1024}} {
		b.Run(shape.name, func(b *testing.B) {
			for _, mode := range []string{"startup-existing-output", "first-materialization"} {
				b.Run(mode, func(b *testing.B) { benchmarkOpenCodeV2Discovery(b, shape, mode) })
			}
		})
	}
}

// The two stores straddle the 4096-entry LRU capacity, with the same one-KiB
// assistant body per session. Sequential discovery must keep every session even
// when the second scan evicts fingerprints before revisiting them.
func BenchmarkDiscoverOpenCodeV2OverCapacity(b *testing.B) {
	for _, sessions := range []int{4096, 4608} {
		shape := openCodeV2BenchmarkShape{fmt.Sprintf("sessions-%d-1KiB", sessions), sessions, 1024}
		b.Run(shape.name, func(b *testing.B) { benchmarkOpenCodeV2Discovery(b, shape, "") })
	}
}

func benchmarkOpenCodeV2Discovery(b *testing.B, shape openCodeV2BenchmarkShape, cold string) {
	b.Helper()
	b.StopTimer()
	roots, cache := openCodeV2BenchmarkCorpus(b, shape)
	var originalTimes map[string]time.Time
	if cold != "first-materialization" {
		originalTimes = make(map[string]time.Time, shape.sessions)
		for i := 0; i < shape.sessions; i++ {
			path := filepath.Join(cache, fmt.Sprintf("ses_%04d.jsonl", i))
			info, err := os.Stat(path)
			if err != nil {
				b.Fatal(err)
			}
			originalTimes[path] = info.ModTime()
		}
	}
	b.ReportAllocs()
	b.SetBytes(int64(shape.sessions * shape.bytes))
	b.ResetTimer()
	b.StartTimer()
	for i := 0; i < b.N; i++ {
		if cold != "" {
			b.StopTimer()
			clearOpenCodeV2BenchmarkFingerprints()
			if cold == "first-materialization" {
				if err := os.RemoveAll(cache); err != nil {
					b.Fatal(err)
				}
			}
			b.StartTimer()
		}
		if files, _, err := Discover(roots, Excluder{}); err != nil || len(files) != shape.sessions {
			b.Fatalf("discovery: files=%d error=%v", len(files), err)
		}
	}
	b.StopTimer()
	verifyOpenCodeV2BenchmarkCorpus(b, shape, cache)
	if originalTimes != nil {
		for path, want := range originalTimes {
			info, err := os.Stat(path)
			if err != nil {
				b.Fatal(err)
			}
			if !info.ModTime().Equal(want) {
				b.Fatalf("unchanged transcript rewritten: %s", path)
			}
		}
		b.ReportMetric(0, "output-rewrites")
	}
}
