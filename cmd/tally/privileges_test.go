//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOwnLeavesOtherFilesAlone(t *testing.T) {
	uid, gid := os.Getuid(), os.Getgid()
	dir := t.TempDir()
	for _, name := range []string{"tally.db", "tally.db-wal", "tally.db-shm", "backup.db", ".tally.db.1.tmp"} {
		os.WriteFile(filepath.Join(dir, name), nil, 0o600)
	}
	if _, err := own(dir, uid, gid); err != nil {
		t.Fatalf("a directory of tally's files: %v", err)
	}
	os.WriteFile(filepath.Join(dir, "notes.txt"), nil, 0o600)
	if _, err := own(dir, uid, gid); err == nil {
		t.Fatal("a directory with other files should be left alone")
	}
	sub := t.TempDir()
	os.Mkdir(filepath.Join(sub, "lost+found"), 0o700)
	if _, err := own(sub, uid, gid); err == nil {
		t.Fatal("a directory with a subdirectory should be left alone")
	}
	fresh := filepath.Join(t.TempDir(), "data")
	if _, err := own(fresh, uid, gid); err != nil {
		t.Fatalf("a directory that does not exist yet is made: %v", err)
	}
}
