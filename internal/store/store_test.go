package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupReplacesTheOldCopy(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "tally.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, out := context.Background(), filepath.Join(dir, "backup.db")
	for i := 0; i < 2; i++ {
		if err := s.Backup(ctx, out); err != nil {
			t.Fatalf("backup %d: %v", i+1, err)
		}
	}
	b, err := Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := b.Entries(ctx, "", ""); err != nil {
		t.Fatalf("the copy should be a working database: %v", err)
	}
	left, _ := filepath.Glob(filepath.Join(dir, ".*tmp"))
	if len(left) > 0 {
		t.Fatalf("temporary files left behind: %v", left)
	}
	_ = os.Remove(out)
}
