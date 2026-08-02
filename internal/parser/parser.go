package parser

import (
	"strings"
	"time"
)

// parseTime parses the timestamp formats these agents emit (RFC3339, with or
// without sub-second precision). A blank or unparseable value yields the zero
// time.
func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// parseMillis converts an epoch-milliseconds integer to a UTC time. OpenCode
// stores every timestamp as a millisecond integer rather than an RFC3339 string,
// so it needs its own converter; a non-positive value (absent, or a zero column)
// yields the zero time exactly as an unparseable string does in parseTime.
func parseMillis(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// toolCategory maps a raw tool name to a coarse category used for filtering and
// stats. Unknown tools get an empty category rather than a guess.
func toolCategory(name string) string {
	switch strings.ToLower(name) {
	case "read", "readfile", "view", "cat":
		return "read"
	case "write", "writefile", "create":
		return "write"
	case "edit", "apply_patch", "applypatch", "str_replace", "multiedit":
		return "edit"
	case "bash", "shell", "shell_command", "exec_command", "run", "execute":
		return "bash"
	case "glob", "grep", "search", "find", "rg":
		return "search"
	default:
		return ""
	}
}
