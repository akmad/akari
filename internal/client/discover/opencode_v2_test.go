package discover

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

// These synthetic tables mirror OpenCode 2.0.29 session/sql.ts. No real database
// or unrelated tables are needed by discovery.
func newOpenCodeV2Fixture(t *testing.T, legacy bool) (string, string) {
	t.Helper()
	dir, cache := t.TempDir(), t.TempDir()
	prev := openCodeCacheDir
	openCodeCacheDir = func(string) (string, error) { return cache, nil }
	t.Cleanup(func() { openCodeCacheDir = prev })
	path := filepath.Join(dir, "opencode.db")
	if legacy {
		writeOpenCodeFixtureDB(t, path, false)
	} else {
		execOpenCodeFixture(t, path, `CREATE TABLE workspace (id TEXT PRIMARY KEY, branch TEXT)`)
	}
	execOpenCodeFixture(t, path, `CREATE TABLE session_v2 (
 id TEXT PRIMARY KEY, parent_id TEXT, workspace_id TEXT, directory TEXT,
 title TEXT, slug TEXT, agent TEXT, model TEXT, time_created INTEGER, time_updated INTEGER);
 CREATE TABLE session_message (id TEXT PRIMARY KEY, session_id TEXT, type TEXT,
 seq INTEGER, time_created INTEGER, time_updated INTEGER, data TEXT);
 INSERT INTO session_v2 VALUES ('ses_v2','ses_parent',NULL,'/project','V2 title','slug','explore','{"id":"gpt-6-sol","providerID":"openai","variant":"high"}',1000,2000);
 INSERT INTO session_message VALUES ('msg_z','ses_v2','user',1,1100,1100,'{"text":"hello","time":{"created":1100}}');
 INSERT INTO session_message VALUES ('msg_a','ses_v2','assistant',2,1200,2000,'{"agent":"explore","model":{"id":"gpt-6-sol","providerID":"openai"},"content":[{"type":"text","text":"latest answer"}],"time":{"created":1200,"completed":2000}}');`)
	return dir, path
}

func TestDiscoverOpenCodeV2(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "v2 only", true: "mixed"}[legacy], func(t *testing.T) {
			dir, _ := newOpenCodeV2Fixture(t, legacy)
			files, _, err := Discover([]Root{{Agent: "opencode", Dir: dir}}, Excluder{})
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if legacy {
				want = 2
			}
			if len(files) != want {
				t.Fatalf("files=%d want %d", len(files), want)
			}
			var raw []byte
			for _, f := range files {
				if filepath.Base(f.Path) == "ses_v2.jsonl" {
					raw, _ = os.ReadFile(f.Path)
				}
			}
			lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
			if len(lines) != 3 {
				t.Fatalf("lines=%d: %s", len(lines), raw)
			}
			if gjson.Get(lines[0], "parent_id").String() != "ses_parent" || gjson.Get(lines[0], "agent").String() != "explore" {
				t.Fatalf("header=%s", lines[0])
			}
			if gjson.Get(lines[1], "id").String() != "msg_z" || gjson.Get(lines[2], "id").String() != "msg_a" {
				t.Fatalf("not seq ordered: %s", raw)
			}
			if gjson.Get(lines[2], "type").String() != "message_v2" || gjson.Get(lines[2], "data.content.0.text").String() != "latest answer" {
				t.Fatalf("message=%s", lines[2])
			}
			info, _ := os.Stat(files[0].Path)
			_, _, err = Discover([]Root{{Agent: "opencode", Dir: dir}}, Excluder{})
			if err != nil {
				t.Fatal(err)
			}
			again, _ := os.Stat(files[0].Path)
			if !info.ModTime().Equal(again.ModTime()) {
				t.Fatal("unchanged session rewritten")
			}
		})
	}
}

func TestDiscoverOpenCodeV2SupersedesMigratedLegacy(t *testing.T) {
	dir, path := newOpenCodeV2Fixture(t, true)
	execOpenCodeFixture(t, path, `UPDATE session_v2 SET id='ses_1'; UPDATE session_message SET session_id='ses_1';`)
	files, _, err := Discover([]Root{{Agent: "opencode", Dir: dir}}, Excluder{})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("files=%d want 1", len(files))
	}
	raw, _ := os.ReadFile(files[0].Path)
	if !strings.Contains(string(raw), "latest answer") || strings.Contains(string(raw), "Fix login") {
		t.Fatalf("stale legacy content: %s", raw)
	}
}

func TestDiscoverOpenCodeV2SettlesWithoutDatabaseUpdate(t *testing.T) {
	dir, path := newOpenCodeV2Fixture(t, false)
	prev := openCodeV2Now
	now := time.UnixMilli(2000)
	openCodeV2Now = func() time.Time { return now }
	t.Cleanup(func() { openCodeV2Now = prev })
	execOpenCodeFixture(t, path, `UPDATE session_message SET data=json_remove(data,'$.time.completed') WHERE type='assistant'; INSERT INTO session_message VALUES ('later','ses_v2','user',3,2100,2100,'{"text":"later prompt","time":{"created":2100}}');`)
	files, _, err := Discover([]Root{{Agent: "opencode", Dir: dir}}, Excluder{})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(files[0].Path)
	if strings.Contains(string(raw), "latest answer") || strings.Contains(string(raw), "later prompt") {
		t.Fatal("mutable turn or later event emitted")
	}
	now = now.Add(openCodeSettleWindow)
	files, _, err = Discover([]Root{{Agent: "opencode", Dir: dir}}, Excluder{})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(files[0].Path)
	if !strings.Contains(string(raw), "latest answer") || !strings.Contains(string(raw), "later prompt") {
		t.Fatal("idle assistant or later event withheld")
	}
}

func TestDiscoverOpenCodeV2RejectsUnknownOrMalformedContent(t *testing.T) {
	for _, data := range []string{`{"agent":"build","model":{"id":"m","providerID":"p"},"content":[{"type":"future"}],"time":{"created":1}}`, `{"agent":"build","model":{"id":"m","providerID":"p"},"content":{},"time":{"created":1}}`, `not json`} {
		t.Run(data, func(t *testing.T) {
			dir, path := newOpenCodeV2Fixture(t, false)
			execOpenCodeFixture(t, path, `UPDATE session_message SET data='`+data+`' WHERE type='assistant'`)
			files, _, err := Discover([]Root{{Agent: "opencode", Dir: dir}}, Excluder{})
			if err == nil {
				t.Fatalf("unsupported content accepted: %+v", files)
			}
			if !strings.Contains(err.Error(), "ses_v2") {
				t.Fatalf("missing session context: %v", err)
			}
		})
	}
}

func TestDiscoverOpenCodeV2EmptyProjectionAndSession(t *testing.T) {
	dir, path := newOpenCodeV2Fixture(t, false)
	execOpenCodeFixture(t, path, `DELETE FROM session_message;
 INSERT INTO session_message VALUES ('switch','ses_v2','model-switched',1,1100,1100,'{"model":{"id":"m","providerID":"p"},"time":{"created":1100}}');
 INSERT INTO session_message VALUES ('empty','ses_v2','assistant',2,1200,1200,'{"agent":"build","model":{"id":"m","providerID":"p"},"content":[],"time":{"created":1200,"completed":1201}}');
 INSERT INTO session_v2 SELECT 'ses_empty',parent_id,workspace_id,directory,title,slug,agent,model,time_created,time_updated FROM session_v2 WHERE id='ses_v2';`)
	files, _, err := Discover([]Root{{Agent: "opencode", Dir: dir}}, Excluder{})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("files=%d", len(files))
	}
	for _, f := range files {
		raw, err := os.ReadFile(f.Path)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		want := 3
		if filepath.Base(f.Path) == "ses_empty.jsonl" {
			want = 1
		}
		if len(lines) != want {
			t.Fatalf("%s: %s", f.Path, raw)
		}
	}
}

func TestDiscoverOpenCodeV2ChecksContentWithUnchangedSessionTime(t *testing.T) {
	dir, path := newOpenCodeV2Fixture(t, false)
	files, _, err := Discover([]Root{{Agent: "opencode", Dir: dir}}, Excluder{})
	if err != nil {
		t.Fatal(err)
	}
	execOpenCodeFixture(t, path, `UPDATE session_message SET data=json_set(data,'$.content[0].text','new answer') WHERE type='assistant'`)
	_, _, err = Discover([]Root{{Agent: "opencode", Dir: dir}}, Excluder{})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(files[0].Path)
	if !strings.Contains(string(raw), "new answer") || strings.Contains(string(raw), "latest answer") {
		t.Fatalf("cached stale content: %s", raw)
	}
}

func TestDiscoverOpenCodeV2RepairsChangedOrMissingCache(t *testing.T) {
	dir, _ := newOpenCodeV2Fixture(t, false)
	root := []Root{{Agent: "opencode", Dir: dir}}
	files, _, err := Discover(root, Excluder{})
	if err != nil {
		t.Fatal(err)
	}
	path := files[0].Path
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, remove := range []bool{false, true} {
		if remove {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		} else {
			// Same length and mtime, but different bytes: cache metadata alone
			// must not allow a damaged transcript to reach ingestion.
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, bytes.Repeat([]byte("x"), len(want)), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, err := Discover(root, Excluder{}); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("cache not restored: error=%v", err)
		}
	}
}

func TestDiscoverOpenCodeV2RefreshesHeaderWithUnchangedSessionTime(t *testing.T) {
	dir, path := newOpenCodeV2Fixture(t, false)
	root := []Root{{Agent: "opencode", Dir: dir}}
	files, _, err := Discover(root, Excluder{})
	if err != nil {
		t.Fatal(err)
	}
	execOpenCodeFixture(t, path, `UPDATE session_v2 SET title='updated title',agent='expert'`)
	if _, _, err := Discover(root, Excluder{}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(files[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	header := strings.SplitN(string(raw), "\n", 2)[0]
	if gjson.Get(header, "title").String() != "updated title" || gjson.Get(header, "agent").String() != "expert" {
		t.Fatalf("cached stale header: %s", header)
	}
}

func TestDiscoverOpenCodeV2RejectsChangedSchemaAndDuplicateSequence(t *testing.T) {
	for _, stmt := range []string{`ALTER TABLE session_message DROP COLUMN seq`, `UPDATE session_message SET seq=1`} {
		t.Run(stmt, func(t *testing.T) {
			dir, path := newOpenCodeV2Fixture(t, false)
			execOpenCodeFixture(t, path, stmt)
			_, _, err := Discover([]Root{{Agent: "opencode", Dir: dir}}, Excluder{})
			if err == nil || !strings.Contains(err.Error(), "seq") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}
