package opencode

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// OpenCode's schema is internal and unversioned: it is not a documented
// interface, and an upgrade can change it without notice. Two defenses stand
// between that and a corrupted backup.
//
// The first is structural and is the real gate. Before a single transcript is
// written, probeSchema checks that the three tables this package reads exist with
// the columns it reads, and that session_message — a next-generation table
// OpenCode has created but not yet populated — is still empty. If OpenCode ever
// switches its writes to session_message, the message and part tables would go
// quiet while still holding a plausible-looking prefix of every session, and
// akari would silently truncate every transcript at the migration boundary. An
// empty table is proof the migration has not happened; a populated one fails the
// probe loudly and the whole root is skipped. Skipping is the correct failure:
// no data is better than half a transcript presented as a whole one.
//
// The second is advisory. minSupportedVersion is the oldest OpenCode whose schema
// has been read and confirmed by hand, and a session written by anything older is
// skipped individually rather than guessed at. There is deliberately no upper
// bound: a newer OpenCode is assumed compatible until the structural probe says
// otherwise, because pinning a ceiling would make every routine upgrade look like
// an outage.
//
// verifiedVersions records the range actually inspected against a live database
// (1.18.5 on two hosts, 1.18.11 on two more, all four with session_message empty
// and an identical session column set). It appears in the probe's failure text so
// a future reader knows what "verified" meant.
const (
	minSupportedVersion = "1.18.5"
	verifiedVersions    = "1.18.5 through 1.18.11"
)

// requiredColumns is every column this package reads, by table. Naming them
// explicitly (rather than trusting a SELECT to fail) means the probe reports a
// schema change as a schema change, before any transcript is written, instead of
// as a query error partway through a rendering.
var requiredColumns = map[string][]string{
	"session": {
		"id", "project_id", "parent_id", "slug", "directory", "title",
		"version", "agent", "model", "time_created", "time_updated",
	},
	"message": {"id", "session_id", "time_created", "data"},
	"part":    {"id", "message_id", "session_id", "time_created", "data"},
}

// probeSchema reports whether the database is one this package can read. A
// non-nil error is a reason to skip the whole root, phrased for a user notice.
func probeSchema(ctx context.Context, db *sql.DB) error {
	for _, table := range []string{"session", "message", "part"} {
		cols, err := tableColumns(ctx, db, table)
		if err != nil {
			return fmt.Errorf("cannot inspect table %q: %w", table, err)
		}
		if len(cols) == 0 {
			return fmt.Errorf("table %q is missing (verified against OpenCode %s)", table, verifiedVersions)
		}
		for _, want := range requiredColumns[table] {
			if !cols[want] {
				return fmt.Errorf("table %q has no column %q, so the schema has changed (verified against OpenCode %s)", table, want, verifiedVersions)
			}
		}
	}

	// session_message is OpenCode's staged replacement for message+part. While it
	// is empty the two are still the source of truth; the moment it has rows they
	// may not be, and reading them would truncate transcripts without any error.
	cols, err := tableColumns(ctx, db, "session_message")
	if err != nil {
		return fmt.Errorf("cannot inspect table %q: %w", "session_message", err)
	}
	if len(cols) > 0 {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM session_message`).Scan(&n); err != nil {
			return fmt.Errorf("cannot count session_message rows: %w", err)
		}
		if n > 0 {
			return fmt.Errorf(
				"session_message holds %d row(s), so OpenCode has migrated off the message/part tables akari reads; "+
					"reading them now would truncate transcripts (verified against OpenCode %s). "+
					"Upgrade akari, or export the sessions manually with `opencode export`", n, verifiedVersions)
		}
	}
	return nil
}

// tableColumns returns the column names of a table as a set, empty when the table
// does not exist. PRAGMA table_info is the portable way to ask both questions at
// once, and it is read-only.
func tableColumns(ctx context.Context, db *sql.DB, table string) (map[string]bool, error) {
	// The table names are compile-time constants from requiredColumns, never user
	// input, so the interpolation cannot carry anything but a fixed identifier.
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

// supportedVersion reports whether a session's recorded OpenCode version is at or
// above the oldest schema that has been verified by hand. A session with no
// version recorded is accepted: the structural probe has already vouched for the
// database, and refusing a row over a blank column would drop real transcripts.
func supportedVersion(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" {
		return true
	}
	return compareVersions(v, minSupportedVersion) >= 0
}

// compareVersions orders two dotted numeric versions ("1.18.11" > "1.18.5"),
// which a string comparison gets wrong. A non-numeric or extra component
// (a pre-release suffix, a fourth number) compares as zero and as an extra
// segment respectively, which is enough for a floor check.
func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		x, y := versionPart(as, i), versionPart(bs, i)
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

func versionPart(parts []string, i int) int {
	if i >= len(parts) {
		return 0
	}
	// Trim any non-numeric tail ("11-beta" -> 11) so a pre-release still orders by
	// its number rather than collapsing to zero.
	s := parts[i]
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	n, err := strconv.Atoi(s[:end])
	if err != nil {
		return 0
	}
	return n
}
