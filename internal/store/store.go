// Package store keeps tally's data in one SQLite file.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure Go, so the binary needs no C toolchain or libc
)

// Entry is one day that differed from a normal day.
type Entry struct {
	Date    string `json:"date"`
	Minutes int    `json:"minutes"`
	Note    string `json:"note"`
}

// Agent is an MCP client that has talked to tally, with the last tool it used.
type Agent struct {
	Name string    `json:"name"`
	Tool string    `json:"tool"`
	At   time.Time `json:"at"`
}

type Store struct{ db *sql.DB }

// migrations run in order, once each. user_version records how many have run.
var migrations = []string{
	`CREATE TABLE entries (
		date TEXT PRIMARY KEY,
		minutes INTEGER NOT NULL CHECK (minutes <> 0),
		note TEXT NOT NULL DEFAULT '',
		updated_at INTEGER NOT NULL
	) STRICT;
	CREATE TABLE settings (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	) STRICT;
	CREATE TABLE sessions (
		id TEXT PRIMARY KEY,
		expires_at INTEGER NOT NULL
	) STRICT;
	CREATE TABLE mcp_sessions (
		id TEXT PRIMARY KEY,
		client TEXT NOT NULL,
		seen_at INTEGER NOT NULL
	) STRICT;
	CREATE TABLE agents (
		name TEXT PRIMARY KEY,
		tool TEXT NOT NULL DEFAULT '',
		seen_at INTEGER NOT NULL
	) STRICT;`,
}

// Open opens or creates the database at path and brings its schema up to date.
func Open(path string) (*Store, error) {
	q := url.Values{}
	for _, p := range []string{"busy_timeout(5000)", "journal_mode(WAL)", "synchronous(NORMAL)", "foreign_keys(1)", "temp_store(MEMORY)"} {
		q.Add("_pragma", p)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	// One connection serialises every read and write. The data is a few hundred rows, so this
	// costs nothing and rules out SQLITE_BUSY and lost updates.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		if dir := filepath.Dir(path); !writable(dir) {
			return nil, fmt.Errorf("open %s: %s is not writable by this user (uid %d). Give it to that user, for the image: chown 65532:65532 on the host directory", path, dir, os.Getuid())
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return s, nil
}

func writable(dir string) bool {
	f, err := os.CreateTemp(dir, ".tally-write-check-*")
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(f.Name())
	return true
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	for ; v < len(migrations); v++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[v]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", v+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, v+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Entries returns every entry from from to to, both included, oldest first. An empty bound is open.
func (s *Store) Entries(ctx context.Context, from, to string) ([]Entry, error) {
	if from == "" {
		from = "0000-00-00"
	}
	if to == "" {
		to = "9999-99-99"
	}
	rows, err := s.db.QueryContext(ctx, `SELECT date, minutes, note FROM entries WHERE date >= ? AND date <= ? ORDER BY date`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.Date, &e.Minutes, &e.Note); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Update reads the entry for date (nil when there is none), lets fn decide what it becomes and
// stores that, all in one transaction. fn returning nil removes the entry.
func (s *Store) Update(ctx context.Context, date string, fn func(old *Entry) (*Entry, error)) (old, updated *Entry, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	var e Entry
	err = tx.QueryRowContext(ctx, `SELECT date, minutes, note FROM entries WHERE date = ?`, date).Scan(&e.Date, &e.Minutes, &e.Note)
	switch {
	case err == nil:
		old = &e
	case !errors.Is(err, sql.ErrNoRows):
		return nil, nil, err
	}
	var in *Entry
	if old != nil {
		c := *old
		in = &c
	}
	updated, err = fn(in)
	if err != nil {
		return nil, nil, err
	}
	if updated == nil || updated.Minutes == 0 {
		updated = nil
		_, err = tx.ExecContext(ctx, `DELETE FROM entries WHERE date = ?`, date)
	} else {
		updated.Date = date
		_, err = tx.ExecContext(ctx, `INSERT INTO entries (date, minutes, note, updated_at) VALUES (?, ?, ?, ?)
			ON CONFLICT (date) DO UPDATE SET minutes = excluded.minutes, note = excluded.note, updated_at = excluded.updated_at`,
			date, updated.Minutes, updated.Note, time.Now().Unix())
	}
	if err != nil {
		return nil, nil, err
	}
	return old, updated, tx.Commit()
}

// Setting returns a stored value, and false when the key has never been set.
func (s *Store) Setting(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// Sessions are browser sign ins. id is a hash of the cookie, never the cookie itself.

func (s *Store) CreateSession(ctx context.Context, id string, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (id, expires_at) VALUES (?, ?)`, id, expires.Unix())
	return err
}

// Session returns when a live session expires, and false when it does not exist or has expired.
func (s *Store) Session(ctx context.Context, id string, now time.Time) (time.Time, bool, error) {
	var exp int64
	err := s.db.QueryRowContext(ctx, `SELECT expires_at FROM sessions WHERE id = ? AND expires_at > ?`, id, now.Unix()).Scan(&exp)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	return time.Unix(exp, 0), err == nil, err
}

func (s *Store) ExtendSession(ctx context.Context, id string, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET expires_at = ? WHERE id = ?`, expires.Unix(), id)
	return err
}

func (s *Store) DeleteSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	return err
}

func (s *Store) DeleteAllSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions`)
	return err
}

// MCP sessions remember which client opened them, so tool calls can be put down to an agent.

func (s *Store) CreateMCPSession(ctx context.Context, id, client string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO mcp_sessions (id, client, seen_at) VALUES (?, ?, ?)`, id, client, now.Unix())
	return err
}

// MCPSession returns the client behind a session and marks it as seen.
func (s *Store) MCPSession(ctx context.Context, id string, now time.Time) (string, bool, error) {
	var client string
	err := s.db.QueryRowContext(ctx, `UPDATE mcp_sessions SET seen_at = ? WHERE id = ? RETURNING client`, now.Unix(), id).Scan(&client)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return client, err == nil, err
}

func (s *Store) DeleteMCPSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM mcp_sessions WHERE id = ?`, id)
	return err
}

func (s *Store) DeleteAllMCPSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM mcp_sessions`)
	return err
}

// SeeAgent records that an agent was active. An empty tool keeps the last tool it used.
func (s *Store) SeeAgent(ctx context.Context, name, tool string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO agents (name, tool, seen_at) VALUES (?, ?, ?)
		ON CONFLICT (name) DO UPDATE SET seen_at = excluded.seen_at, tool = CASE WHEN excluded.tool = '' THEN agents.tool ELSE excluded.tool END`,
		name, tool, now.Unix())
	return err
}

// Agents lists every agent that has connected, the most recent first.
func (s *Store) Agents(ctx context.Context) ([]Agent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, tool, seen_at FROM agents ORDER BY seen_at DESC, name LIMIT 20`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Agent{}
	for rows.Next() {
		var a Agent
		var at int64
		if err := rows.Scan(&a.Name, &a.Tool, &at); err != nil {
			return nil, err
		}
		a.At = time.Unix(at, 0).UTC()
		out = append(out, a)
	}
	return out, rows.Err()
}

// Prune drops expired sign ins, MCP sessions idle for a month and agents not seen for half a year.
func (s *Store) Prune(ctx context.Context, now time.Time) error {
	for _, q := range []struct {
		sql    string
		before time.Time
	}{
		{`DELETE FROM sessions WHERE expires_at <= ?`, now},
		{`DELETE FROM mcp_sessions WHERE seen_at <= ?`, now.AddDate(0, 0, -30)},
		{`DELETE FROM agents WHERE seen_at <= ?`, now.AddDate(0, -6, 0)},
	} {
		if _, err := s.db.ExecContext(ctx, q.sql, q.before.Unix()); err != nil {
			return err
		}
	}
	return nil
}

// Backup writes a consistent copy of the database to path, replacing any file already there.
// The copy is made next to path first and then renamed, so path is never half written.
func (s *Store) Backup(ctx context.Context, path string) error {
	tmp := filepath.Join(filepath.Dir(path), fmt.Sprintf(".%s.%d.tmp", filepath.Base(path), os.Getpid()))
	os.Remove(tmp)
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
