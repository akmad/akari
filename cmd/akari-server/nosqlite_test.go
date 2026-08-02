package main

import (
	"os/exec"
	"strings"
	"testing"
)

// TestServerLinksNoSQLite pins a structural boundary: OpenCode support reads a
// SQLite database, and that reading happens entirely in the client. This binary
// is a CGO_ENABLED=0 cross-compile into a distroless static base, and its only
// database is Postgres.
//
// The pure-Go driver would not break the build if it leaked in — that is exactly
// why this test exists. It would leak in silently, adding megabytes and a second
// storage engine's worth of surface to the server, and nothing else would
// complain. The likely accident is a shared client package growing an import of
// internal/client/opencode: discovery already resolves the OpenCode cache root,
// and internal/devseed pulls discovery into this binary.
func TestServerLinksNoSQLite(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	for _, dep := range strings.Fields(string(out)) {
		if strings.HasPrefix(dep, "modernc.org/sqlite") ||
			dep == "github.com/jssblck/akari/internal/client/opencode" {
			t.Errorf("the server binary depends on %s; SQLite belongs to the client only "+
				"(this binary cross-compiles with CGO_ENABLED=0 into a static distroless image, "+
				"and its only database is Postgres)", dep)
		}
	}
}
