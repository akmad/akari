package opencode

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/tidwall/gjson"
)

// The synthetic transcript is newline-delimited JSON with one record per line, so
// it behaves like a Claude or pi transcript rather than a Codex one: every newline
// is a message boundary the uploader can chunk on.
//
//	{"type":"session","id":…,"slug":…,"title":…,"directory":…,"projectID":…,
//	 "parentID":…,"agent":…,"model":{…},"version":…,"timeCreated":…}
//	{"type":"message","id":"msg_…","role":"user","timeCreated":…,"data":{…}}
//	{"type":"part","id":"prt_…","messageID":"msg_…","timeCreated":…,"data":{…}}
//
// Two properties matter more than anything else in this file.
//
// Determinism: the same database state must render to the same bytes, every run,
// on every machine. Field order is hand-written rather than produced by a struct
// or a map, integers are emitted verbatim rather than formatted, and the message
// and part payloads are the database's own JSON passed through json.Compact,
// which strips insignificant whitespace without reordering or re-escaping
// anything. Nothing derived, timestamped, or locally-flavored enters the output.
//
// Append-only growth: the rendering of a session must always be a byte-prefix of
// its later renderings, because akari's uploader verifies the prefix it has
// already sent and re-uploads the whole file when it does not match. Everything
// mutable is therefore withheld until it has settled — see complete() — and the
// session's own running totals (tokens, cost), which change on every turn, are
// never emitted at all. The per-message token counts are the source of truth
// anyway, so nothing is lost.
//
// When growth is not append-only despite that (a session retitled after the fact,
// a message deleted), the file is replaced atomically and akari's existing
// prefix-mismatch recovery re-uploads it cleanly. That path is correct, just
// expensive, which is why the withholding rules are conservative.

// sessionRow is the subset of the session table the header line carries. The
// running totals (tokens_*, cost) are deliberately absent: they change on every
// turn, so emitting them would rewrite the first line of the file constantly.
type sessionRow struct {
	ID          string
	ProjectID   string
	ParentID    string
	Slug        string
	Directory   string
	Title       string
	Version     string
	Agent       string
	Model       string // raw JSON from the column, or "null"
	TimeCreated int64
	TimeUpdated int64
}

type messageRow struct {
	ID          string
	TimeCreated int64
	Data        string
}

type partRow struct {
	ID          string
	MessageID   string
	TimeCreated int64
	Data        string
}

// listSessions reads every session's header fields, ordered by id so a run's work
// order does not depend on the database's physical layout.
func listSessions(ctx context.Context, db *sql.DB) ([]sessionRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, project_id, parent_id, slug, directory, title, version, agent, model,
		       time_created, time_updated
		  FROM session
		 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []sessionRow
	for rows.Next() {
		var s sessionRow
		var parent, agent, model sql.NullString
		if err := rows.Scan(&s.ID, &s.ProjectID, &parent, &s.Slug, &s.Directory, &s.Title,
			&s.Version, &agent, &model, &s.TimeCreated, &s.TimeUpdated); err != nil {
			return nil, err
		}
		s.ParentID, s.Agent = parent.String, agent.String
		s.Model = "null"
		if model.Valid && model.String != "" {
			s.Model = model.String
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// syncSession renders one session and reconciles it with its cached file,
// reporting whether a transcript for the session now exists on disk.
//
// Everything runs inside one deferred read transaction, so the freshness check,
// the session row, its messages and its parts all come from a single SQLite
// snapshot. Without that, a turn completing mid-render could be seen half
// written, and the rendering would not be reproducible from any one state of the
// database.
//
// The freshness check is what keeps a routine sync cheap: reading a session's
// payloads costs megabytes, and latestChange decides whether to bother for the
// price of two indexed aggregates.
func syncSession(ctx context.Context, db *sql.DB, s sessionRow, path string) (bool, error) {
	info, statErr := os.Stat(path)
	exists := statErr == nil
	if !exists && !errors.Is(statErr, fs.ErrNotExist) {
		return false, statErr
	}

	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false, fmt.Errorf("begin read snapshot: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // read-only; nothing to fail

	if exists {
		changed, err := latestChange(ctx, tx, s)
		if err != nil {
			return false, fmt.Errorf("read session freshness: %w", err)
		}
		if !time.UnixMilli(changed).After(info.ModTime()) {
			return true, nil
		}
	}

	rendered, err := renderSession(ctx, tx, s)
	if err != nil {
		return false, err
	}
	if len(rendered) == 0 {
		// Nothing has settled yet. Leave any existing file alone rather than
		// truncating a transcript that was complete on an earlier pass.
		return exists, nil
	}
	if err := writeTranscript(path, rendered); err != nil {
		return false, err
	}
	return true, nil
}

// latestChange returns the most recent write to anything the transcript renders,
// in epoch milliseconds. A file is always written after the rows it renders were
// read, so its mtime is at or after this value at that moment, and a later
// OpenCode write pushes the value past it.
//
// All three tables have to be consulted. session.time_updated alone looks like
// the obvious signal and is not: on a real 61-session database every single
// session had a message written after its session row was last touched, five of
// them by more than a minute and one by eighteen hours. Gating on the session row
// alone would freeze those transcripts permanently. Erring the other way is
// harmless — a coarse filesystem timestamp only makes the file look older than it
// is, which costs one extra render that produces identical bytes.
func latestChange(ctx context.Context, tx *sql.Tx, s sessionRow) (int64, error) {
	var newest int64
	err := tx.QueryRowContext(ctx, `
		SELECT max(t) FROM (
			SELECT ? AS t
			UNION ALL SELECT coalesce(max(time_updated), 0) FROM message WHERE session_id = ?
			UNION ALL SELECT coalesce(max(time_updated), 0) FROM part    WHERE session_id = ?
		)`, s.TimeUpdated, s.ID, s.ID).Scan(&newest)
	return newest, err
}

// renderSession produces the whole transcript for one session, or nil when the
// session has nothing settled enough to publish.
func renderSession(ctx context.Context, tx *sql.Tx, s sessionRow) ([]byte, error) {
	messages, err := readMessages(ctx, tx, s.ID)
	if err != nil {
		return nil, fmt.Errorf("read messages: %w", err)
	}
	parts, err := readParts(ctx, tx, s.ID)
	if err != nil {
		return nil, fmt.Errorf("read parts: %w", err)
	}

	var buf bytes.Buffer
	writeSessionLine(&buf, s)

	settled := false
	for _, m := range messages {
		mine := parts[m.ID]
		if !complete(m, mine) {
			// Stop at the first unsettled message rather than skipping it: emitting a
			// later message now and this one after it settles would insert bytes in the
			// middle of the file, which is exactly the non-append-only rewrite the
			// uploader has to recover from.
			break
		}
		if err := writeMessageLine(&buf, m); err != nil {
			return nil, fmt.Errorf("message %s: %w", m.ID, err)
		}
		for _, p := range mine {
			if err := writePartLine(&buf, p); err != nil {
				return nil, fmt.Errorf("part %s: %w", p.ID, err)
			}
		}
		if gjson.Get(m.Data, "role").String() == "assistant" {
			settled = true
		}
	}

	// Hold the whole session back until its first assistant turn has landed. Before
	// that, OpenCode is still generating the session's title asynchronously, so the
	// header line is guaranteed to change and publishing early would guarantee a
	// rewrite. A titleless session is in that same window.
	if !settled || s.Title == "" {
		return nil, nil
	}
	return buf.Bytes(), nil
}

// complete reports whether a message and its parts have stopped changing.
//
// A user turn is written once and never revised, so it is complete on sight. An
// assistant turn is rewritten in place for as long as it runs: its data gains
// time.completed only at the end, and each tool part's state advances through
// "running" to "completed" or "error" with its output filled in on the way. Both
// conditions have to hold, because a turn can record its completion while a tool
// part it spawned is still being finalized.
//
// This is the rule that keeps a live OpenCode session from causing an upload
// reset on every sync, so it errs toward withholding: an unparseable payload
// counts as unsettled.
func complete(m messageRow, parts []partRow) bool {
	if !gjson.Valid(m.Data) {
		return false
	}
	if gjson.Get(m.Data, "role").String() != "assistant" {
		return true
	}
	if !gjson.Get(m.Data, "time.completed").Exists() {
		return false
	}
	for _, p := range parts {
		if !gjson.Valid(p.Data) {
			return false
		}
		if gjson.Get(p.Data, "type").String() != "tool" {
			continue
		}
		switch gjson.Get(p.Data, "state.status").String() {
		case "completed", "error":
		default:
			return false
		}
	}
	return true
}

func readMessages(ctx context.Context, tx *sql.Tx, sessionID string) ([]messageRow, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, time_created, data FROM message
		 WHERE session_id = ? ORDER BY time_created, id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []messageRow
	for rows.Next() {
		var m messageRow
		if err := rows.Scan(&m.ID, &m.TimeCreated, &m.Data); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// readParts returns every part of a session grouped by its message, each group in
// (time_created, id) order. The sort is done here rather than in SQL so the order
// cannot depend on the database's collation.
func readParts(ctx context.Context, tx *sql.Tx, sessionID string) (map[string][]partRow, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, message_id, time_created, data FROM part
		 WHERE session_id = ?`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]partRow{}
	for rows.Next() {
		var p partRow
		if err := rows.Scan(&p.ID, &p.MessageID, &p.TimeCreated, &p.Data); err != nil {
			return nil, err
		}
		out[p.MessageID] = append(out[p.MessageID], p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, group := range out {
		sort.Slice(group, func(i, j int) bool {
			if group[i].TimeCreated != group[j].TimeCreated {
				return group[i].TimeCreated < group[j].TimeCreated
			}
			return group[i].ID < group[j].ID
		})
	}
	return out, nil
}

// writeSessionLine emits the header. The field order is fixed here, in one place,
// because it is part of the on-disk format: changing it rewrites every cached
// transcript and forces a full re-upload of every OpenCode session.
func writeSessionLine(buf *bytes.Buffer, s sessionRow) {
	buf.WriteString(`{"type":"session","id":`)
	writeJSONString(buf, s.ID)
	buf.WriteString(`,"slug":`)
	writeJSONString(buf, s.Slug)
	buf.WriteString(`,"title":`)
	writeJSONString(buf, s.Title)
	buf.WriteString(`,"directory":`)
	writeJSONString(buf, s.Directory)
	buf.WriteString(`,"projectID":`)
	writeJSONString(buf, s.ProjectID)
	buf.WriteString(`,"parentID":`)
	writeJSONString(buf, s.ParentID)
	buf.WriteString(`,"agent":`)
	writeJSONString(buf, s.Agent)
	buf.WriteString(`,"model":`)
	// The model column is already JSON; compacting keeps it on one line without
	// reinterpreting it. An unparseable value degrades to null rather than
	// breaking the line.
	if err := json.Compact(buf, []byte(s.Model)); err != nil {
		buf.WriteString("null")
	}
	buf.WriteString(`,"version":`)
	writeJSONString(buf, s.Version)
	buf.WriteString(`,"timeCreated":`)
	buf.WriteString(strconv.FormatInt(s.TimeCreated, 10))
	buf.WriteString("}\n")
}

func writeMessageLine(buf *bytes.Buffer, m messageRow) error {
	buf.WriteString(`{"type":"message","id":`)
	writeJSONString(buf, m.ID)
	buf.WriteString(`,"role":`)
	writeJSONString(buf, gjson.Get(m.Data, "role").String())
	buf.WriteString(`,"timeCreated":`)
	buf.WriteString(strconv.FormatInt(m.TimeCreated, 10))
	buf.WriteString(`,"data":`)
	if err := json.Compact(buf, []byte(m.Data)); err != nil {
		return fmt.Errorf("payload is not JSON: %w", err)
	}
	buf.WriteString("}\n")
	return nil
}

func writePartLine(buf *bytes.Buffer, p partRow) error {
	buf.WriteString(`{"type":"part","id":`)
	writeJSONString(buf, p.ID)
	buf.WriteString(`,"messageID":`)
	writeJSONString(buf, p.MessageID)
	buf.WriteString(`,"timeCreated":`)
	buf.WriteString(strconv.FormatInt(p.TimeCreated, 10))
	buf.WriteString(`,"data":`)
	if err := json.Compact(buf, []byte(p.Data)); err != nil {
		return fmt.Errorf("payload is not JSON: %w", err)
	}
	buf.WriteString("}\n")
	return nil
}

// writeJSONString appends a JSON string literal with HTML escaping off, so a
// value containing <, > or & is encoded the same way every other JSON writer in
// the pipeline would encode it. encoding/json's default escaping would be
// deterministic too, but it would differ from the raw payloads passing through
// json.Compact beside it, and one file should not use two conventions.
func writeJSONString(buf *bytes.Buffer, s string) {
	b, err := marshalString(s)
	if err != nil {
		buf.WriteString(`""`)
		return
	}
	buf.Write(b)
}

func marshalString(s string) ([]byte, error) {
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return nil, err
	}
	// Encode appends a newline the literal must not carry.
	return bytes.TrimRight(out.Bytes(), "\n"), nil
}

// writeTranscript reconciles the freshly rendered bytes with the cached file,
// taking the cheapest correct path: nothing when they already match, an append
// when the file is a strict prefix of the rendering, and an atomic replacement
// otherwise. The append is what makes an ordinary growing session cost one small
// write per sync instead of rewriting the file, and it is also what lets the
// uploader treat the cache exactly like an agent's own append-only log.
func writeTranscript(path string, want []byte) error {
	matched, isPrefix, err := comparePrefix(path, want)
	if err != nil {
		return err
	}
	switch {
	case isPrefix && matched == int64(len(want)):
		return nil
	case isPrefix:
		// O_CREATE covers the first render, where the "prefix" matched is the empty
		// file that does not exist yet; every later render appends to a real file.
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			return err
		}
		if _, err := f.Write(want[matched:]); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	default:
		return replaceFile(path, want)
	}
}

// comparePrefix reports how many leading bytes of the cached file equal want, and
// whether the file ended exactly at that point (making it a prefix of want). It
// streams the comparison in bounded windows so a large transcript is never held
// twice in memory. A missing file is an empty prefix.
func comparePrefix(path string, want []byte) (matched int64, isPrefix bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, true, nil
		}
		return 0, false, err
	}
	defer f.Close()

	const window = 64 << 10
	buf := make([]byte, window)
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			if matched+int64(n) > int64(len(want)) {
				return matched, false, nil // the file is longer than the rendering
			}
			if !bytes.Equal(buf[:n], want[matched:matched+int64(n)]) {
				return matched, false, nil
			}
			matched += int64(n)
		}
		if readErr == io.EOF {
			return matched, true, nil
		}
		if readErr != nil {
			return matched, false, readErr
		}
	}
}

// replaceFile writes the rendering to a temporary file beside the target and
// renames it into place, so a crash or a full disk can never leave a half-written
// transcript for the uploader to read. The temporary name is derived from the
// target rather than randomized, so a run interrupted mid-write leaves at most one
// stale file per session instead of accumulating them.
func replaceFile(path string, content []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, content, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
