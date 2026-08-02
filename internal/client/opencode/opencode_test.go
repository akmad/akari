package opencode

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The fixtures below are built row by row rather than copied from a real
// database: a transcript is user content, and the point of these tests is the
// rendering rules, not anyone's actual session. The identities follow the rest of
// the suite (women in computing history).

// newDB creates an empty SQLite database with OpenCode's schema, at a real path
// so the read-only open path under test is the one exercised. It returns a writer
// handle for the test to populate and the path Materialize should be pointed at.
func newDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	schema := []string{
		`CREATE TABLE session (
			id TEXT PRIMARY KEY, project_id TEXT NOT NULL, workspace_id TEXT, parent_id TEXT,
			slug TEXT NOT NULL, directory TEXT NOT NULL, path TEXT, title TEXT NOT NULL,
			version TEXT NOT NULL, share_url TEXT, cost REAL DEFAULT 0 NOT NULL,
			tokens_input INTEGER DEFAULT 0 NOT NULL, tokens_output INTEGER DEFAULT 0 NOT NULL,
			tokens_reasoning INTEGER DEFAULT 0 NOT NULL, tokens_cache_read INTEGER DEFAULT 0 NOT NULL,
			tokens_cache_write INTEGER DEFAULT 0 NOT NULL, agent TEXT, model TEXT,
			time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL)`,
		`CREATE TABLE message (
			id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER NOT NULL,
			time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
		`CREATE TABLE part (
			id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL,
			time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
		`CREATE TABLE session_message (
			id TEXT PRIMARY KEY, session_id TEXT NOT NULL, type TEXT NOT NULL, seq INTEGER NOT NULL,
			time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
	}
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("create schema: %v", err)
		}
	}
	return db, path
}

func exec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %s: %v", query, err)
	}
}

const (
	sessionID = "ses_hopper01"
	version   = "1.18.7"
)

// OpenCode timestamps are epoch milliseconds, and the materializer compares them
// against the cache file's modification time to decide whether a re-render is
// needed. Fixture times are therefore anchored to now: a fixture written in 1970
// would look permanently older than any file it produced, and every test would
// silently exercise the skip path instead of the rendering.
var baseMillis = time.Now().UnixMilli()

// at returns a fixture timestamp offset seconds from the run's anchor.
func at(offsetSeconds int64) int64 { return baseMillis + offsetSeconds*1000 }

// addSession inserts the header row every fixture starts from.
func addSession(t *testing.T, db *sql.DB, updated int64) {
	t.Helper()
	exec(t, db, `INSERT INTO session
		(id, project_id, parent_id, slug, directory, title, version, agent, model, time_created, time_updated)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		sessionID, "proj_a", "", "brisk-compiler", "/home/grace/code/nav",
		"Trace the nav regression", version, "build",
		`{"id":"gpt-5.6-sol","providerID":"openai","variant":"high"}`, 1000, updated)
}

func touchSession(t *testing.T, db *sql.DB, updated int64) {
	t.Helper()
	exec(t, db, `UPDATE session SET time_updated = ? WHERE id = ?`, updated, sessionID)
}

func addMessage(t *testing.T, db *sql.DB, id string, created int64, data string) {
	t.Helper()
	exec(t, db, `INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?,?,?,?,?)`,
		id, sessionID, created, created, data)
}

func addPart(t *testing.T, db *sql.DB, id, messageID string, created int64, data string) {
	t.Helper()
	exec(t, db, `INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES (?,?,?,?,?,?)`,
		id, messageID, sessionID, created, created, data)
}

// userTurn is a settled user message: OpenCode writes one once and never revises it.
func userTurn(t *testing.T, db *sql.DB, id string, created int64, text string) {
	t.Helper()
	addMessage(t, db, id, created, fmt.Sprintf(`{"role":"user","time":{"created":%d},"agent":"build"}`, created))
	addPart(t, db, "prt_"+id, id, created+1, fmt.Sprintf(`{"type":"text","text":%q}`, text))
}

// completedTurn is a settled assistant message with one finished tool call.
func completedTurn(t *testing.T, db *sql.DB, id string, created int64, answer string) {
	t.Helper()
	addMessage(t, db, id, created, fmt.Sprintf(
		`{"role":"assistant","agent":"build","cost":0,`+
			`"tokens":{"input":10,"output":2,"reasoning":0,"cache":{"read":0,"write":0}},`+
			`"modelID":"gpt-5.6-sol","providerID":"openai",`+
			`"time":{"created":%d,"completed":%d},"finish":"stop"}`, created, created+100))
	addPart(t, db, "prt_"+id+"_a", id, created+1,
		`{"type":"tool","tool":"read","callID":"call_`+id+`","state":{"status":"completed","input":{"file_path":"nav.go"},"output":"package nav\n"}}`)
	addPart(t, db, "prt_"+id+"_b", id, created+2, fmt.Sprintf(`{"type":"text","text":%q}`, answer))
}

func materialize(t *testing.T, dbPath, cacheDir string) ([]string, []string) {
	t.Helper()
	files, notices, err := Materialize(context.Background(), dbPath, cacheDir)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	return files, notices
}

func readTranscript(t *testing.T, cacheDir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(cacheDir, sessionID+".jsonl"))
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	return string(b)
}

// TestMaterializeIsDeterministic is the property everything else rests on: the
// same database state renders to the same bytes. If it did not, every sync would
// see a changed file, fail the uploader's prefix check, and re-upload the session
// from scratch.
func TestMaterializeIsDeterministic(t *testing.T) {
	db, dbPath := newDB(t)
	addSession(t, db, at(0))
	userTurn(t, db, "msg_1", at(1), "why does nav drift?")
	completedTurn(t, db, "msg_2", at(2), "the heading is stale")

	first := t.TempDir()
	files, notices := materialize(t, dbPath, first)
	if len(files) != 1 {
		t.Fatalf("files = %v, want one transcript (notices=%v)", files, notices)
	}
	if len(notices) != 0 {
		t.Errorf("unexpected notices: %v", notices)
	}
	a := readTranscript(t, first)

	// A second run into a fresh directory must produce byte-identical output: the
	// rendering may not depend on anything but the database.
	second := t.TempDir()
	materialize(t, dbPath, second)
	if b := readTranscript(t, second); a != b {
		t.Errorf("rendering is not deterministic:\nfirst:\n%s\nsecond:\n%s", a, b)
	}

	// Re-running over the existing cache must also leave it exactly as it was.
	materialize(t, dbPath, first)
	if c := readTranscript(t, first); a != c {
		t.Errorf("re-run changed the cached transcript:\nbefore:\n%s\nafter:\n%s", a, c)
	}

	// Shape: one header, and every line is its own JSON record.
	lines := strings.Split(strings.TrimSuffix(a, "\n"), "\n")
	if len(lines) != 6 {
		t.Fatalf("lines = %d, want 6 (header + 2 messages + 3 parts)\n%s", len(lines), a)
	}
	if !strings.HasPrefix(lines[0], `{"type":"session","id":"`+sessionID+`"`) {
		t.Errorf("header line = %s", lines[0])
	}
	// The session's running totals must never appear: they move every turn, so
	// emitting them would rewrite the header constantly.
	for _, banned := range []string{"tokens_input", "tokens_cache_read", `"cost"`} {
		if strings.Contains(lines[0], banned) {
			t.Errorf("header carries the mutable field %s: %s", banned, lines[0])
		}
	}
}

// TestMaterializeWithholdsInFlightTurn covers the steady state on a machine that
// is using OpenCode right now: the newest turn is still running. Publishing it
// would mean rewriting those bytes when it finishes, which the uploader can only
// recover from by re-uploading the whole session.
func TestMaterializeWithholdsInFlightTurn(t *testing.T) {
	db, dbPath := newDB(t)
	addSession(t, db, at(0))
	userTurn(t, db, "msg_1", at(1), "why does nav drift?")
	completedTurn(t, db, "msg_2", at(2), "the heading is stale")
	// A turn still streaming: no time.completed yet.
	addMessage(t, db, "msg_3", at(3),
		`{"role":"assistant","agent":"build","tokens":{"input":1,"output":0,"reasoning":0,"cache":{"read":0,"write":0}},`+
			`"modelID":"gpt-5.6-sol","time":{"created":1300}}`)
	addPart(t, db, "prt_msg_3_a", "msg_3", at(4), `{"type":"text","text":"looking"}`)

	cache := t.TempDir()
	materialize(t, dbPath, cache)
	got := readTranscript(t, cache)
	if strings.Contains(got, "msg_3") {
		t.Errorf("in-flight turn was published:\n%s", got)
	}

	// The same holds for a turn that reports itself complete while one of its tool
	// calls is still running: the part's output is still being written.
	exec(t, db, `UPDATE message SET data = ? WHERE id = 'msg_3'`,
		`{"role":"assistant","agent":"build","tokens":{"input":1,"output":0,"reasoning":0,"cache":{"read":0,"write":0}},`+
			`"modelID":"gpt-5.6-sol","time":{"created":1300,"completed":1400}}`)
	addPart(t, db, "prt_msg_3_b", "msg_3", at(5),
		`{"type":"tool","tool":"bash","callID":"call_3","state":{"status":"running","input":{"command":"go test ./..."}}}`)
	touchSession(t, db, at(10))

	materialize(t, dbPath, cache)
	if got := readTranscript(t, cache); strings.Contains(got, "msg_3") {
		t.Errorf("turn with a running tool call was published:\n%s", got)
	}

	// Once the tool settles, the whole turn appears.
	exec(t, db, `UPDATE part SET data = ? WHERE id = 'prt_msg_3_b'`,
		`{"type":"tool","tool":"bash","callID":"call_3","state":{"status":"completed","input":{"command":"go test ./..."},"output":"ok\n"}}`)
	touchSession(t, db, at(20))
	materialize(t, dbPath, cache)
	if got := readTranscript(t, cache); !strings.Contains(got, "msg_3") {
		t.Errorf("settled turn was not published:\n%s", got)
	}
}

// TestMaterializeAppendsSettledTurn is the growth invariant: a session that gains
// a turn must render to a strict byte extension of what it rendered before, so the
// uploader sends only the delta and never re-sends the session.
func TestMaterializeAppendsSettledTurn(t *testing.T) {
	db, dbPath := newDB(t)
	addSession(t, db, at(0))
	userTurn(t, db, "msg_1", at(1), "why does nav drift?")
	completedTurn(t, db, "msg_2", at(2), "the heading is stale")

	cache := t.TempDir()
	materialize(t, dbPath, cache)
	before := readTranscript(t, cache)

	userTurn(t, db, "msg_3", at(3), "fix it")
	completedTurn(t, db, "msg_4", at(4), "done")
	touchSession(t, db, at(30))

	materialize(t, dbPath, cache)
	after := readTranscript(t, cache)
	if !strings.HasPrefix(after, before) {
		t.Fatalf("growth was not append-only.\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if len(after) <= len(before) {
		t.Errorf("transcript did not grow: %d -> %d bytes", len(before), len(after))
	}
}

// TestMaterializeReplacesOnNonExtension covers the other side of the same
// invariant: when history changes underneath (a message deleted, a session
// retitled), the cache must be replaced outright. Leaving stale bytes would make
// the server's copy diverge from the source of truth; the uploader detects the
// changed prefix and re-uploads cleanly.
func TestMaterializeReplacesOnNonExtension(t *testing.T) {
	db, dbPath := newDB(t)
	addSession(t, db, at(0))
	userTurn(t, db, "msg_1", at(1), "why does nav drift?")
	completedTurn(t, db, "msg_2", at(2), "the heading is stale")
	userTurn(t, db, "msg_3", at(3), "and the roll?")
	completedTurn(t, db, "msg_4", at(4), "also stale")

	cache := t.TempDir()
	materialize(t, dbPath, cache)
	before := readTranscript(t, cache)

	exec(t, db, `DELETE FROM part WHERE message_id = 'msg_2'`)
	exec(t, db, `DELETE FROM message WHERE id = 'msg_2'`)
	touchSession(t, db, at(40))

	materialize(t, dbPath, cache)
	after := readTranscript(t, cache)
	if strings.HasPrefix(after, before) || strings.Contains(after, "msg_2") {
		t.Errorf("deleted message survived the re-render:\n%s", after)
	}
	if !strings.Contains(after, "msg_4") {
		t.Errorf("re-render lost the surviving turns:\n%s", after)
	}
}

// TestMaterializeWithholdsUntitledSession pins the rule that keeps the very first
// sync of a new session from being wasted: OpenCode generates the title
// asynchronously after the opening exchange, and the title is on the header line,
// so publishing before it lands guarantees the file is rewritten.
func TestMaterializeWithholdsUntitledSession(t *testing.T) {
	db, dbPath := newDB(t)
	addSession(t, db, at(0))
	exec(t, db, `UPDATE session SET title = '' WHERE id = ?`, sessionID)
	userTurn(t, db, "msg_1", at(1), "hello")
	completedTurn(t, db, "msg_2", at(2), "hi")

	cache := t.TempDir()
	files, _ := materialize(t, dbPath, cache)
	if len(files) != 0 {
		t.Errorf("untitled session was published: %v", files)
	}

	// A session with a title but no completed assistant turn is withheld too.
	db2, dbPath2 := newDB(t)
	addSession(t, db2, at(0))
	userTurn(t, db2, "msg_1", at(1), "hello")
	if files, _ := materialize(t, dbPath2, t.TempDir()); len(files) != 0 {
		t.Errorf("session with no settled assistant turn was published: %v", files)
	}
}

// TestMaterializePrunesDepartedSessions keeps the cache tracking the database
// rather than growing forever, and covers the temporary file replaceFile may
// leave behind.
func TestMaterializePrunesDepartedSessions(t *testing.T) {
	db, dbPath := newDB(t)
	addSession(t, db, at(0))
	userTurn(t, db, "msg_1", at(1), "hello")
	completedTurn(t, db, "msg_2", at(2), "hi")

	cache := t.TempDir()
	materialize(t, dbPath, cache)
	stale := filepath.Join(cache, "ses_lovelace.jsonl")
	if err := os.WriteFile(stale, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	staleTmp := filepath.Join(cache, "ses_lovelace.jsonl.tmp")
	if err := os.WriteFile(staleTmp, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A file that is not a transcript at all is left alone.
	keep := filepath.Join(cache, "README")
	if err := os.WriteFile(keep, []byte("not ours\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	materialize(t, dbPath, cache)
	for _, gone := range []string{stale, staleTmp} {
		if _, err := os.Stat(gone); err == nil {
			t.Errorf("%s survived pruning", filepath.Base(gone))
		}
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("pruning removed an unrelated file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache, sessionID+".jsonl")); err != nil {
		t.Errorf("pruning removed a live session's transcript: %v", err)
	}
}

// TestMaterializeMissingDatabaseIsQuiet: not having OpenCode installed is the
// common case, not a problem to report.
func TestMaterializeMissingDatabaseIsQuiet(t *testing.T) {
	files, notices, err := Materialize(context.Background(),
		filepath.Join(t.TempDir(), "absent.db"), t.TempDir())
	if err != nil || len(files) != 0 || len(notices) != 0 {
		t.Errorf("missing database: files=%v notices=%v err=%v, want all empty", files, notices, err)
	}
}

// TestMaterializeSkipsMigratedSchema is risk R1: OpenCode has created a
// next-generation session_message table but does not write to it yet. If it ever
// starts, message and part hold a stale prefix of every session and reading them
// would silently truncate transcripts. The probe must refuse the whole database.
func TestMaterializeSkipsMigratedSchema(t *testing.T) {
	db, dbPath := newDB(t)
	addSession(t, db, at(0))
	userTurn(t, db, "msg_1", at(1), "hello")
	completedTurn(t, db, "msg_2", at(2), "hi")
	exec(t, db, `INSERT INTO session_message (id, session_id, type, seq, time_created, time_updated, data)
		VALUES ('sm_1', ?, 'message', 1, 1100, 1100, '{}')`, sessionID)

	cache := t.TempDir()
	files, notices := materialize(t, dbPath, cache)
	if len(files) != 0 {
		t.Errorf("a migrated database still produced transcripts: %v", files)
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "session_message") {
		t.Fatalf("notices = %v, want one naming session_message", notices)
	}
	if entries, _ := os.ReadDir(cache); len(entries) != 0 {
		t.Errorf("a skipped database wrote %d file(s); it must write none", len(entries))
	}
}

// TestMaterializeSkipsMissingColumns: a schema that lost a column akari reads is
// reported as a schema change before anything is written, not as a query error
// halfway through a rendering.
func TestMaterializeSkipsMissingColumns(t *testing.T) {
	db, dbPath := newDB(t)
	exec(t, db, `DROP TABLE part`)
	exec(t, db, `CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL)`)

	files, notices := materialize(t, dbPath, t.TempDir())
	if len(files) != 0 {
		t.Errorf("files = %v, want none", files)
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "has no column") {
		t.Errorf("notices = %v, want one naming the missing column", notices)
	}
}

// TestMaterializeSkipsUnsupportedVersion: a session older than the oldest schema
// anyone has verified is skipped individually, with the rest of the database
// still materialized.
func TestMaterializeSkipsUnsupportedVersion(t *testing.T) {
	db, dbPath := newDB(t)
	addSession(t, db, at(0))
	userTurn(t, db, "msg_1", at(1), "hello")
	completedTurn(t, db, "msg_2", at(2), "hi")
	exec(t, db, `UPDATE session SET version = '1.17.9' WHERE id = ?`, sessionID)

	files, notices := materialize(t, dbPath, t.TempDir())
	if len(files) != 0 {
		t.Errorf("files = %v, want none", files)
	}
	if len(notices) != 1 || !strings.Contains(notices[0], minSupportedVersion) {
		t.Errorf("notices = %v, want one naming the minimum supported version", notices)
	}
}

func TestSupportedVersion(t *testing.T) {
	// 1.18.11 must read as newer than 1.18.5, which a string comparison gets wrong.
	for _, v := range []string{"1.18.5", "1.18.11", "1.19.0", "2.0.0", "1.18.11-beta", ""} {
		if !supportedVersion(v) {
			t.Errorf("version %q should be supported", v)
		}
	}
	for _, v := range []string{"1.18.4", "1.17.20", "0.9.0"} {
		if supportedVersion(v) {
			t.Errorf("version %q should not be supported", v)
		}
	}
}

func TestDBPathHonorsOverrides(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	if got := DBPath(env(map[string]string{DBEnvVar: "/custom/oc.db"}), "/home/grace"); got != "/custom/oc.db" {
		t.Errorf("explicit override = %q", got)
	}
	want := filepath.Join("/data", "opencode", "opencode.db")
	if got := DBPath(env(map[string]string{"XDG_DATA_HOME": "/data"}), "/home/grace"); got != want {
		t.Errorf("XDG override = %q, want %q", got, want)
	}
	want = filepath.Join("/home/grace", ".local", "share", "opencode", "opencode.db")
	if got := DBPath(env(nil), "/home/grace"); got != want {
		t.Errorf("default = %q, want %q", got, want)
	}
}

// TestMaterializeSkipsUnchangedSession pins the freshness gate. Reading a
// session's payloads costs megabytes, so a pass over an untouched session must
// not do it — but the gate has to consult the message and part tables, not just
// the session row, because on a real database every session had a message written
// after its session row was last touched.
func TestMaterializeSkipsUnchangedSession(t *testing.T) {
	db, dbPath := newDB(t)
	addSession(t, db, at(0))
	userTurn(t, db, "msg_1", at(1), "why does nav drift?")
	completedTurn(t, db, "msg_2", at(2), "the heading is stale")

	cache := t.TempDir()
	materialize(t, dbPath, cache)
	path := filepath.Join(cache, sessionID+".jsonl")

	// Age everything well behind the file so the gate should decline to re-render.
	past := time.Now().Add(-time.Hour).UnixMilli()
	exec(t, db, `UPDATE session SET time_updated = ?`, past)
	exec(t, db, `UPDATE message SET time_updated = ?`, past)
	exec(t, db, `UPDATE part SET time_updated = ?`, past)
	// Corrupt a payload: a render would either fail or produce different bytes, so
	// an unchanged file proves no render happened.
	exec(t, db, `UPDATE part SET data = '{"type":"text","text":"CHANGED"}' WHERE id = 'prt_msg_1'`)

	files, notices := materialize(t, dbPath, cache)
	if len(files) != 1 || len(notices) != 0 {
		t.Errorf("files=%v notices=%v, want the session reported and no notices", files, notices)
	}
	if got := readTranscript(t, cache); strings.Contains(got, "CHANGED") {
		t.Errorf("an unchanged session was re-rendered:\n%s", got)
	}

	// A message written after the file, with the session row still stale, must
	// re-render: this is the case session.time_updated alone would miss.
	exec(t, db, `UPDATE message SET time_updated = ? WHERE id = 'msg_2'`, time.Now().Add(time.Minute).UnixMilli())
	materialize(t, dbPath, cache)
	if got := readTranscript(t, cache); !strings.Contains(got, "CHANGED") {
		t.Errorf("a session whose message moved was not re-rendered:\n%s", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("transcript vanished: %v", err)
	}
}
