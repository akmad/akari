package main

import (
	"context"
	"os"

	"github.com/jssblck/akari/internal/client/discover"
	"github.com/jssblck/akari/internal/client/opencode"
)

// materializeOpencode renders the OpenCode SQLite database into the cache
// directory discovery walks as the "opencode" root, returning the notices the
// caller should surface.
//
// It runs immediately before every discovery pass, because unlike every other
// agent OpenCode writes no session files of its own: the cache is the transcript,
// and a pass that skipped this would discover whatever the previous pass left. It
// is deliberately best-effort — a machine with no OpenCode installed, a database
// that cannot be opened, or a schema this version does not recognize all yield
// notices and an untouched cache, never a failed sync of the other agents' work.
// Only a local failure (the cache directory itself being unusable) is an error,
// and it is reported the same way a discovery failure is.
func materializeOpencode(ctx context.Context) (notices []string, err error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	cacheDir, err := discover.OpencodeCacheDir(os.Getenv, os.UserCacheDir)
	if err != nil {
		return nil, err
	}
	_, notices, err = opencode.Materialize(ctx, opencode.DBPath(os.Getenv, home), cacheDir)
	return notices, err
}
