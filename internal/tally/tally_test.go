package tally

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"tally/internal/store"
)

// newService opens a fresh database where today is Sunday 18 Oct 2026 in Stockholm.
func newService(t *testing.T) *Service {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "tally.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	loc, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	s := New(st, loc)
	s.Now = func() time.Time { return time.Date(2026, 10, 18, 12, 0, 0, 0, loc) }
	return s
}

func TestFormat(t *testing.T) {
	for min, want := range map[int]string{90: "+1h 30m", -45: "−45m", 120: "+2h", 0: "0m", -480: "−8h", 15: "+15m", 61: "+1h 1m"} {
		if got := Format(min); got != want {
			t.Errorf("Format(%d) = %q, want %q", min, got, want)
		}
	}
}

func TestDates(t *testing.T) {
	if got := StartOf("week", "2026-10-18"); got != "2026-10-12" {
		t.Errorf("week of a Sunday starts %s, want the Monday before", got)
	}
	if got := StartOf("quarter", "2026-12-31"); got != "2026-10-01" {
		t.Errorf("quarter start = %s", got)
	}
	if got := ISOWeek("2026-10-12"); got != "2026-W42" {
		t.Errorf("ISOWeek = %s", got)
	}
	if got := ISOWeek("2027-01-01"); got != "2026-W53" {
		t.Errorf("1 Jan 2027 belongs to the last week of 2026, got %s", got)
	}
	for _, bad := range []string{"2026-02-30", "2026-1-5", "20261005", "", "2026-10-05T00:00"} {
		if _, err := ParseDate(bad); err == nil {
			t.Errorf("ParseDate(%q) should fail", bad)
		}
	}
}

func TestAddFollowsTheRules(t *testing.T) {
	s, ctx := newService(t), context.Background()
	day := "2026-10-16"

	ch, err := s.Add(ctx, day, 30, "Release")
	if err != nil || ch.Before != nil || ch.After.Minutes != 30 || ch.After.Note != "Release" {
		t.Fatalf("first add: %+v %v", ch, err)
	}
	// Saving onto a day adds to it, and an empty note keeps the old one.
	ch, err = s.Add(ctx, day, 45, "")
	if err != nil || ch.After.Minutes != 75 || ch.After.Note != "Release" {
		t.Fatalf("second add: %+v %v", ch.After, err)
	}
	// A new note replaces the old one.
	ch, _ = s.Add(ctx, day, -15, "  On call ")
	if ch.After.Minutes != 60 || ch.After.Note != "On call" {
		t.Fatalf("third add: %+v", ch.After)
	}
	// An entry that reaches 0 is removed.
	ch, _ = s.Add(ctx, day, -60, "")
	if ch.After != nil {
		t.Fatalf("a day at 0 should be removed, got %+v", ch.After)
	}
	if all, _ := s.Entries(ctx); len(all) != 0 {
		t.Fatalf("entries left: %+v", all)
	}
}

func TestAddRejects(t *testing.T) {
	s, ctx := newService(t), context.Background()
	cases := map[string]func() error{
		"tomorrow": func() error { _, err := s.Add(ctx, "2026-10-19", 15, ""); return err },
		"bad date": func() error { _, err := s.Add(ctx, "2026-13-01", 15, ""); return err },
		"zero":     func() error { _, err := s.Add(ctx, "2026-10-18", 0, ""); return err },
		"huge":     func() error { _, err := s.Add(ctx, "2026-10-18", MaxAdd+1, ""); return err },
		"long note": func() error {
			_, err := s.Add(ctx, "2026-10-18", 15, "12345678901234567890123456789012345678901")
			return err
		},
		"set future": func() error { _, err := s.Set(ctx, "2026-10-19", 15, ""); return err },
	}
	for name, fn := range cases {
		if err := fn(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
	// 40 characters is fine, counted as characters rather than bytes.
	if _, err := s.Add(ctx, "2026-10-18", 15, "åäöåäöåäöåäöåäöåäöåäöåäöåäöåäöåäöåäöåäöå"); err != nil {
		t.Errorf("40 characters: %v", err)
	}
}

func TestToday(t *testing.T) {
	s := newService(t)
	// 23:30 UTC on the 17th is already the 18th in Stockholm.
	s.Now = func() time.Time { return time.Date(2026, 10, 17, 23, 30, 0, 0, time.UTC) }
	if got := s.Today(); got != "2026-10-18" {
		t.Errorf("Today = %s, want the day in the service's zone", got)
	}
}

func TestDemoMatchesTheReference(t *testing.T) {
	s, ctx := newService(t), context.Background()
	n, err := s.Demo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := s.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The design's screenshots: +7h 25m over 66 days, this week 0m, month and quarter +55m.
	if n != 66 || sum.Balance.Text != "+7h 25m" || sum.Week.Text != "0m" || sum.Month.Text != "+55m" || sum.Quarter.Text != "+55m" {
		t.Fatalf("demo: %d days, %+v", n, sum)
	}
	if _, err := s.Demo(ctx); err == nil {
		t.Fatal("demo should refuse a database that has entries")
	}
}

func TestDaysAndTrend(t *testing.T) {
	s, ctx := newService(t), context.Background()
	for _, e := range []struct {
		date string
		min  int
	}{{"2026-09-28", 60}, {"2026-10-01", 30}, {"2026-10-02", -90}, {"2026-10-12", 45}} {
		if _, err := s.Add(ctx, e.date, e.min, ""); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.Days(ctx, "2026-10-01", "")
	if err != nil {
		t.Fatal(err)
	}
	if list.To != "2026-10-18" || list.DaysLogged != 3 || list.Change.Minutes != -15 || list.Days[0].Balance.Minutes != 90 {
		t.Fatalf("days: %+v", list)
	}
	trend, err := s.Trend(ctx, "2026-09-01", "")
	if err != nil {
		t.Fatal(err)
	}
	// Weeks before the first entry are skipped. 28 Sep is a Monday.
	if len(trend.Weeks) != 3 || trend.From != "2026-09-28" {
		t.Fatalf("trend: %+v", trend)
	}
	w := trend.Weeks[0]
	if w.Week != "2026-W40" || w.Change.Minutes != 0 || w.More.Minutes != 90 || w.Less.Minutes != -90 || w.Balance.Minutes != 0 {
		t.Fatalf("first week: %+v", w)
	}
	if last := trend.Weeks[2]; last.Balance.Minutes != 45 || last.To != "2026-10-18" {
		t.Fatalf("last week: %+v", last)
	}
	if _, err := s.Days(ctx, "2026-10-10", "2026-10-01"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("from after to should be invalid, got %v", err)
	}
}

func TestRestoreOnlyWhenUnchanged(t *testing.T) {
	s, ctx := newService(t), context.Background()
	day := "2026-10-16"
	s.Add(ctx, day, 30, "Phone")
	// The browser saves 15m on top, then an agent adds 45m before the browser's Undo arrives.
	ch, _ := s.Add(ctx, day, 15, "")
	s.Add(ctx, day, 45, "")
	if _, err := s.Restore(ctx, day, ch.Before, ch.After); !errors.Is(err, ErrConflict) {
		t.Fatalf("undo over a later change should conflict, got %v", err)
	}
	if all, _ := s.Entries(ctx); all[0].Minutes != 90 {
		t.Fatalf("a conflicting undo must change nothing: %+v", all)
	}
	// Without the agent in between, undo puts the day back.
	ch, _ = s.Add(ctx, day, -90, "")
	if ch.After != nil {
		t.Fatalf("day should be gone: %+v", ch.After)
	}
	if res, err := s.Restore(ctx, day, ch.Before, ch.After); err != nil || res.After.Minutes != 90 || res.After.Note != "Phone" {
		t.Fatalf("undo of a delete: %+v %v", res, err)
	}
}

func TestTrendDefaultsToTwelveWeeks(t *testing.T) {
	s, ctx := newService(t), context.Background()
	s.Add(ctx, "2026-01-05", 30, "")
	for _, to := range []string{"2026-10-16", "2026-10-18", "2026-10-12"} {
		trend, err := s.Trend(ctx, "", to)
		if err != nil || len(trend.Weeks) != 12 || trend.Weeks[11].From != Monday(to) {
			t.Errorf("to %s: %d weeks from %s, %v", to, len(trend.Weeks), trend.From, err)
		}
	}
}
