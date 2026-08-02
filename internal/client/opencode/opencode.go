// Package opencode materializes OpenCode sessions out of OpenCode's SQLite
// database and into a cache of ordinary JSONL transcripts, one file per session,
// which discovery then walks as a fourth agent root.
//
// Every other agent akari backs up writes its transcript to disk as it goes, so
// the client's job is only to find the file and stream the bytes the server has
// not seen. OpenCode instead keeps everything in a single SQLite database, so
// this package supplies the missing artifact: a deterministic, append-only
// rendering of each session that behaves exactly like a Claude or pi transcript.
// Everything downstream of discovery (resolution, the announce/verify/upload
// protocol, the server's reducer) is unchanged, and in particular the server
// gains no SQLite dependency: only the client ever opens the database.
//
// The rendering is a pure function of the database — there is no sidecar state
// and nothing is remembered between runs. Each pass re-renders a session and
// byte-compares it against the cached file: an unchanged file is left alone, a
// strict extension is appended, and anything else atomically replaces the file
// (which makes the next sync's prefix verification fail and triggers akari's
// existing clean re-upload path). That is what makes the cache safe to delete at
// any time and safe to share between `akari sync` and `akari watch`.
//
// The database is opened strictly read-only and is never checkpointed, never
// written, and never opened with immutable=1 (which would be unsound against a
// live writer). OpenCode's account/credential tables hold OAuth tokens and are
// never read: only session, message, and part are.
package opencode

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite" // pure-Go driver: the client must not need cgo either
)

// DBEnvVar overrides where OpenCode's database is looked for. It exists because
// only the macOS and Linux defaults have been verified; a user whose install
// differs points akari at it rather than going unsupported. The matching override
// for where the transcripts are written is discover.OpencodeCacheEnvVar, which
// lives beside the root that reads them.
const DBEnvVar = "AKARI_OPENCODE_DB"

// DBPath returns the OpenCode database to read. The override wins; otherwise it
// is the XDG data location OpenCode uses ($XDG_DATA_HOME, else ~/.local/share).
func DBPath(env func(string) string, home string) string {
	if p := strings.TrimSpace(env(DBEnvVar)); p != "" {
		return p
	}
	data := strings.TrimSpace(env("XDG_DATA_HOME"))
	if data == "" {
		data = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(data, "opencode", "opencode.db")
}

// Materialize renders every eligible OpenCode session into cacheDir and returns
// the transcript paths it wrote or confirmed, together with any non-fatal
// notices the caller should surface.
//
// A missing database is not a condition at all: OpenCode simply is not installed
// (or is installed elsewhere), so the pass returns nothing, quietly, exactly as a
// missing built-in discovery root does. A database that exists but cannot be used
// — locked, unreadable, or carrying a schema this version does not recognize —
// yields a notice and no files, never a partial rendering. err is reserved for a
// local failure the user must fix: the cache directory itself being unusable.
func Materialize(ctx context.Context, dbPath, cacheDir string) (files []string, notices []string, err error) {
	if _, statErr := os.Stat(dbPath); statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, []string{fmt.Sprintf("opencode: cannot read %s: %v; skipping", dbPath, statErr)}, nil
	}

	db, openErr := openReadOnly(dbPath)
	if openErr != nil {
		// A live WAL database still needs the reader to create or attach its shared
		// index, so a read-only open can fail for reasons that are not about akari.
		// Report it and move on rather than failing a sync that has claude, codex
		// and pi work to do.
		return nil, []string{fmt.Sprintf("opencode: cannot open %s read-only: %v; skipping", dbPath, openErr)}, nil
	}
	defer db.Close()

	if probeErr := probeSchema(ctx, db); probeErr != nil {
		return nil, []string{"opencode: " + probeErr.Error() + "; skipping (no partial transcripts are written)"}, nil
	}

	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create opencode cache dir %s: %w", cacheDir, err)
	}

	sessions, err := listSessions(ctx, db)
	if err != nil {
		return nil, []string{fmt.Sprintf("opencode: cannot list sessions in %s: %v; skipping", dbPath, err)}, nil
	}

	var (
		out          []string
		note         []string
		unsupported  int
		oldestSeen   string
		renderErrors int
	)
	live := make(map[string]bool, len(sessions))
	for _, s := range sessions {
		if err := ctx.Err(); err != nil {
			return out, note, nil
		}
		live[s.ID] = true
		if !supportedVersion(s.Version) {
			unsupported++
			if oldestSeen == "" || s.Version < oldestSeen {
				oldestSeen = s.Version
			}
			continue
		}
		path := filepath.Join(cacheDir, s.ID+".jsonl")
		written, err := syncSession(ctx, db, s, path)
		if err != nil {
			renderErrors++
			note = append(note, fmt.Sprintf("opencode: session %s not materialized: %v", s.ID, err))
			continue
		}
		if written {
			out = append(out, path)
		}
	}
	if unsupported > 0 {
		note = append(note, fmt.Sprintf(
			"opencode: skipped %d session(s) written by OpenCode %s or older; akari supports %s and newer",
			unsupported, oldestSeen, minSupportedVersion))
	}
	if pruned, err := prune(cacheDir, live); err != nil {
		note = append(note, fmt.Sprintf("opencode: cannot prune stale transcripts in %s: %v", cacheDir, err))
	} else if pruned > 0 {
		note = append(note, fmt.Sprintf("opencode: removed %d transcript(s) for sessions no longer in the database", pruned))
	}
	sort.Strings(out)
	return out, note, nil
}

// openReadOnly opens the database in the only mode this package ever uses. The
// three settings are all load-bearing: mode=ro opens the file read-only,
// query_only makes the connection refuse a write even if a future code path asked
// for one, and a deferred transaction lock keeps a reader from ever taking the
// write lock a live OpenCode process needs. immutable=1 is deliberately absent:
// it promises SQLite the file cannot change, which is false for a database being
// written right now, and would return torn reads rather than a consistent
// snapshot.
func openReadOnly(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+uriEscapePath(path)+"?mode=ro&_pragma=query_only(1)&_txlock=deferred")
	if err != nil {
		return nil, err
	}
	// sql.Open is lazy, so force a real connection here: an unusable database must
	// be reported by Materialize as a notice, not surface later as a query error
	// halfway through a rendering.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// uriEscapePath percent-encodes the characters SQLite's URI parser would
// otherwise read as syntax, so a database under a path containing '?', '#' or
// '%' still opens. Everything else is left alone: SQLite accepts a raw path in a
// file: URI, and escaping the separators would break it.
func uriEscapePath(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case '?', '#', '%':
			fmt.Fprintf(&b, "%%%02X", c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// prune deletes cached transcripts whose session is no longer in the database, so
// the cache tracks OpenCode rather than growing forever. Only files this package
// writes are considered: the directory is akari's own, and a name that is not
// "<session id>.jsonl" is left untouched.
func prune(cacheDir string, live map[string]bool) (int, error) {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		name := e.Name()
		// Both the transcript and the temporary file replaceFile writes beside it
		// belong to a session, so a departed session takes both with it.
		base, ok := strings.CutSuffix(name, ".jsonl")
		if !ok {
			if base, ok = strings.CutSuffix(name, ".jsonl.tmp"); !ok {
				continue
			}
		}
		if e.IsDir() || live[base] {
			continue
		}
		if err := os.Remove(filepath.Join(cacheDir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return n, err
		}
		n++
	}
	return n, nil
}
