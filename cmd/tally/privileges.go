//go:build linux

package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// dropRoot runs first. When tally starts as root and PUID is set, as it is in the image, it
// makes its data directory its own and then becomes the user PUID:PGID for good. A bind mount
// that Docker created owned by root, or one owned by someone else, then works without a chown
// on the host. Started as any other user, or as root without PUID, nothing changes.
func dropRoot(log *slog.Logger) error {
	if os.Getuid() != 0 || os.Getenv("PUID") == "" {
		return nil
	}
	uid, err := strconv.Atoi(os.Getenv("PUID"))
	if err != nil || uid < 0 {
		return fmt.Errorf("PUID=%s is not a user id", os.Getenv("PUID"))
	}
	gid := uid
	if s := os.Getenv("PGID"); s != "" {
		if gid, err = strconv.Atoi(s); err != nil || gid < 0 {
			return fmt.Errorf("PGID=%s is not a group id", s)
		}
	}
	if uid == 0 {
		return nil // asked to stay root
	}
	dir := filepath.Dir(env("TALLY_DB", "tally.db"))
	if changed, err := own(dir, uid, gid); err != nil {
		log.Warn("the data directory is left as it is", "dir", dir, "reason", err)
	} else if changed {
		log.Info("gave the data directory to the user tally runs as", "dir", dir, "uid", uid, "gid", gid)
	}
	// Group first, while there is still the right to change it, then the user.
	if err := syscall.Setgroups([]int{}); err != nil {
		return dropErr(uid, gid, err)
	}
	if err := syscall.Setgid(gid); err != nil {
		return dropErr(uid, gid, err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return dropErr(uid, gid, err)
	}
	if os.Getuid() != uid || os.Geteuid() != uid {
		return fmt.Errorf("still running as %d after switching to %d", os.Geteuid(), uid)
	}
	return nil
}

func dropErr(uid, gid int, err error) error {
	return fmt.Errorf("cannot switch from root to %d:%d (%w). If the container drops all capabilities, run it as that user instead, with user: \"%d:%d\", and give the data directory to that user", uid, gid, err, uid, gid)
}

// own gives dir, and tally's files in it, to uid:gid. It changes nothing in a directory that
// holds anything but tally's files: that is more likely the wrong mount than tally's data.
func own(dir string, uid, gid int) (changed bool, err error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return false, err
	}
	paths := []string{dir}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || !tallyFile(e.Name()) {
			return false, fmt.Errorf("it holds %q, which is not tally's. Give it to %d:%d yourself if it is the right directory", e.Name(), uid, gid)
		}
		paths = append(paths, filepath.Join(dir, e.Name()))
	}
	for _, p := range paths {
		fi, err := os.Lstat(p)
		if err != nil {
			return changed, err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if ok && int(st.Uid) == uid && int(st.Gid) == gid {
			continue
		}
		if err := os.Lchown(p, uid, gid); err != nil {
			return changed, fmt.Errorf("cannot change its owner: %w", err)
		}
		changed = true
	}
	return changed, nil
}

// tallyFile says whether a name is one tally writes: the database with its journal files, a
// backup, or a backup being written.
func tallyFile(name string) bool {
	for _, end := range []string{".db", ".db-wal", ".db-shm", ".db-journal", ".tmp"} {
		if strings.HasSuffix(name, end) {
			return true
		}
	}
	return false
}
