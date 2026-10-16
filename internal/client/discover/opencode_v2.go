package discover

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jssblck/akari/internal/parser"
)

var openCodeV2Now = time.Now

// Remember only fingerprints, never transcript contents. Every hit still hashes
// the exact database rows and cached output; timestamps cannot hide an edit or
// a damaged cache. The bound keeps long-running watchers' memory predictable.
var openCodeV2Fingerprints = struct {
	sync.Mutex
	entries map[string]*list.Element
	order   list.List
}{entries: make(map[string]*list.Element)}

type openCodeV2Fingerprint struct {
	path           string
	source, output [sha256.Size]byte
}

func rememberOpenCodeV2Fingerprint(value openCodeV2Fingerprint) {
	cache := &openCodeV2Fingerprints
	cache.Lock()
	defer cache.Unlock()
	if entry := cache.entries[value.path]; entry != nil {
		entry.Value = value
		cache.order.MoveToFront(entry)
		return
	}
	cache.entries[value.path] = cache.order.PushFront(value)
	if cache.order.Len() > 4096 {
		oldest := cache.order.Back()
		delete(cache.entries, oldest.Value.(openCodeV2Fingerprint).path)
		cache.order.Remove(oldest)
	}
}

func unchangedOpenCodeV2Fingerprint(path string, source [sha256.Size]byte) bool {
	cache := &openCodeV2Fingerprints
	cache.Lock()
	entry := cache.entries[path]
	if entry == nil || entry.Value.(openCodeV2Fingerprint).source != source {
		cache.Unlock()
		return false
	}
	value := entry.Value.(openCodeV2Fingerprint)
	cache.order.MoveToFront(entry)
	cache.Unlock()
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return false
	}
	return bytes.Equal(hash.Sum(nil), value.output[:])
}

func hashOpenCodeV2Source(tx *sql.Tx, sess openCodeSession, stale bool) ([sha256.Size]byte, error) {
	hash := sha256.New()
	// JSON length framing is unambiguous even when IDs or message bytes contain
	// separators. Include the settle decision, so an idle mutable turn becomes
	// publishable without requiring a database write.
	metadata, err := json.Marshal(struct {
		Session openCodeSession
		Stale   bool
	}{sess, stale})
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	fmt.Fprintf(hash, "%d:", len(metadata))
	hash.Write(metadata)
	rows, err := tx.Query(`SELECT id,type,seq,time_created,data FROM session_message WHERE session_id=? ORDER BY seq,id`, sess.ID)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, typ string
		var seq, created int64
		var raw []byte
		if err := rows.Scan(&id, &typ, &seq, &created, &raw); err != nil {
			return [sha256.Size]byte{}, err
		}
		fmt.Fprintf(hash, "%d:%s%d:%s%d:%d:%d:", len(id), id, len(typ), typ, seq, created, len(raw))
		hash.Write(raw)
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest, rows.Err()
}

// The transcript contract is pinned to OpenCode 2.0.30 session/sql.ts and
// schema/session-message.ts. Legacy tables survive migration but stop updating.
func probeOpenCodeV2Schema(db openCodeQuerier) (bool, error) {
	cols, err := openCodeTableColumns(db, "session_v2")
	if err != nil {
		return false, err
	}
	if len(cols) == 0 {
		return false, nil
	}
	for table, wants := range map[string][]string{
		"session_v2":      openCodeColumns["session"],
		"workspace":       openCodeColumns["workspace"],
		"session_message": {"id", "session_id", "type", "seq", "time_created", "time_updated", "data"},
	} {
		cs, err := openCodeTableColumns(db, table)
		if err != nil {
			return false, err
		}
		for _, col := range wants {
			if !cs[col] {
				return false, fmt.Errorf("table %q has no column %q, so the v2 schema has changed", table, col)
			}
		}
	}
	return true, nil
}

func listV2OpenCodeSessions(db openCodeQuerier) ([]openCodeSession, error) {
	branch, err := openCodeWorkspaceBranch(db)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(fmt.Sprintf(`SELECT s.id,coalesce(s.parent_id,''),coalesce(s.directory,''),coalesce(s.title,''),coalesce(s.slug,''),%s,coalesce(s.agent,''),s.model,s.time_created,s.time_updated FROM session_v2 s LEFT JOIN workspace w ON w.id=s.workspace_id ORDER BY s.time_created,s.id`, branch))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []openCodeSession
	for rows.Next() {
		var s openCodeSession
		var model sql.NullString
		if err := rows.Scan(&s.ID, &s.ParentID, &s.Directory, &s.Title, &s.Slug, &s.Branch, &s.Agent, &model, &s.TimeCreated, &s.TimeUpdated); err != nil {
			return nil, err
		}
		if s.ID == "" || filepath.Base(s.ID) != s.ID || s.ID == "." || s.ID == ".." {
			return nil, fmt.Errorf("invalid v2 session ID")
		}
		if model.Valid && model.String != "null" && model.String != "" {
			if !json.Valid([]byte(model.String)) {
				return nil, fmt.Errorf("invalid model on v2 session %s", s.ID)
			}
			s.Model = []byte(model.String)
		}
		s.V2 = true
		out = append(out, s)
	}
	return out, rows.Err()
}

type openCodeV2MessageLine struct {
	Type        string          `json:"type"`
	ID          string          `json:"id"`
	TimeCreated int64           `json:"time_created"`
	Data        json.RawMessage `json:"data"`
}

func materializeV2OpenCodeSession(db *sql.DB, sess openCodeSession, path string) (bool, error) {
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	stale := openCodeV2Now().Sub(time.UnixMilli(sess.TimeUpdated)) >= openCodeSettleWindow
	fingerprint, err := hashOpenCodeV2Source(tx, sess, stale)
	if err != nil {
		return false, err
	}
	if unchangedOpenCodeV2Fingerprint(path, fingerprint) {
		return true, tx.Commit()
	}
	rows, err := tx.Query(`SELECT id,type,seq,time_created,data FROM session_message WHERE session_id=? ORDER BY seq,id`, sess.ID)
	if err != nil {
		return false, err
	}
	var messages []openCodeV2MessageLine
	var lastSeq int64
	var haveSeq bool
	for rows.Next() {
		var m openCodeV2MessageLine
		var typ string
		var seq int64
		var raw []byte
		if err := rows.Scan(&m.ID, &typ, &seq, &m.TimeCreated, &raw); err != nil {
			rows.Close()
			return false, err
		}
		if haveSeq && seq <= lastSeq {
			rows.Close()
			return false, fmt.Errorf("v2 message %s has duplicate or unordered seq %d", m.ID, seq)
		}
		lastSeq = seq
		haveSeq = true
		var data map[string]json.RawMessage
		if err := json.Unmarshal(raw, &data); err != nil || data == nil {
			rows.Close()
			return false, fmt.Errorf("invalid v2 message %s JSON", m.ID)
		}
		data["type"], _ = json.Marshal(typ)
		encoded, err := json.Marshal(data)
		if err != nil {
			rows.Close()
			return false, err
		}
		if err := parser.ValidateOpenCodeV2Message(encoded); err != nil {
			rows.Close()
			return false, fmt.Errorf("v2 message %s: %w; refusing truncated transcript", m.ID, err)
		}
		m.Type = "message_v2"
		m.Data = encoded
		messages = append(messages, m)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	rows.Close()
	if err := tx.Commit(); err != nil {
		return false, err
	}
	var buf bytes.Buffer
	if err := writeOpenCodeJSONL(&buf, sess, nil, nil, false); err != nil {
		return false, err
	}
	enc := json.NewEncoder(&buf)
	for _, m := range messages {
		// Preserve a stable prefix: later events cannot jump ahead of a mutable turn.
		if !openCodeV2MessageReady(m.Data, stale) {
			break
		}
		if err := enc.Encode(m); err != nil {
			return false, err
		}
	}
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, buf.Bytes()) {
		rememberOpenCodeV2Fingerprint(openCodeV2Fingerprint{path, fingerprint, sha256.Sum256(buf.Bytes())})
		return true, nil
	}
	outputFingerprint := sha256.Sum256(buf.Bytes())
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp)
	if _, err := buf.WriteTo(f); err != nil {
		f.Close()
		return false, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return false, err
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return false, err
	}
	rememberOpenCodeV2Fingerprint(openCodeV2Fingerprint{path, fingerprint, outputFingerprint})
	return true, nil
}

func openCodeV2MessageReady(data []byte, stale bool) bool {
	var m struct {
		Type   string `json:"type"`
		Status string `json:"status"`
		Time   struct {
			Completed int64 `json:"completed"`
		} `json:"time"`
		Error json.RawMessage `json:"error"`
	}
	_ = json.Unmarshal(data, &m)
	switch m.Type {
	case "assistant":
		return m.Time.Completed > 0 || len(m.Error) > 0 || stale
	case "shell", "compaction":
		return m.Status != "running" || stale
	}
	return true
}
