package tally

import (
	"context"
	"errors"
	"math"
	"time"
)

// Demo fills an empty database with the sample data from the design reference, moved forward
// by whole weeks so it ends just before today and every date keeps its weekday.
func (s *Service) Demo(ctx context.Context) (int, error) {
	if existing, err := s.Store.Entries(ctx, "", ""); err != nil {
		return 0, err
	} else if len(existing) > 0 {
		return 0, errors.New("the database already has entries, demo data only goes into an empty one")
	}
	ref := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	today, _ := ParseDate(s.Today())
	shift := int(math.Floor(today.Sub(ref).Hours()/24/7)) * 7
	n := 0
	for _, e := range demoEntries() {
		d, _ := ParseDate(e.date)
		date := formatDate(d.AddDate(0, 0, shift))
		if _, err := s.Set(ctx, date, e.min, e.note); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

type demoEntry struct {
	date string
	min  int
	note string
}

// demoEntries is a straight port of design/reference/data.js, seed and all.
func demoEntries() []demoEntry {
	seed := uint32(11)
	rnd := func() float64 {
		seed = seed*1664525 + 1013904223
		return float64(seed) / 4294967296
	}
	type fixed struct {
		min  int
		note string
	}
	fixedDays := map[string]fixed{
		"2026-08-14": {-240, "Comp"}, "2026-08-28": {-480, "Day off"},
		"2026-10-01": {45, ""}, "2026-10-02": {-60, "School"}, "2026-10-05": {90, "Deploy"},
		"2026-10-07": {30, ""}, "2026-10-09": {-45, "Dentist"},
	}
	overNotes := []string{"Release", "Deploy", "On call", "Workshop", "Travel", "Review", "", "", "", ""}
	underNotes := []string{"Dentist", "School", "Errand", "", "", ""}
	// month: chance of an entry, share that are over, largest size in quarter hours
	phase := map[time.Month][3]float64{
		time.April: {.4, .35, 6}, time.May: {.45, .65, 6}, time.June: {.7, .88, 12},
		time.July: {.3, .6, 5}, time.August: {.35, .45, 6}, time.September: {.45, .62, 7}, time.October: {0, 0, 0},
	}
	leaveFrom, leaveTo := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC), time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
	var out []demoEntry
	for d := time.Date(2026, 4, 6, 0, 0, 0, 0, time.UTC); d.Before(end); d = d.AddDate(0, 0, 1) {
		if d.Weekday() == time.Saturday || d.Weekday() == time.Sunday {
			continue
		}
		key := formatDate(d)
		if f, ok := fixedDays[key]; ok {
			out = append(out, demoEntry{key, f.min, f.note})
			continue
		}
		if !d.Before(leaveFrom) && !d.After(leaveTo) {
			continue // summer leave
		}
		p := phase[d.Month()]
		if rnd() > p[0] {
			continue
		}
		sign, notes := -1, underNotes
		if rnd() < p[1] {
			sign, notes = 1, overNotes
		}
		q := 1 + int(math.Floor(math.Pow(rnd(), 1.8)*p[2]))
		out = append(out, demoEntry{key, sign * q * 15, notes[int(math.Floor(rnd()*float64(len(notes))))]})
	}
	// A few typed amounts, so the lists show what free form looks like.
	typed := map[string]demoEntry{
		"2026-10-05": {"2026-10-05", 80, "Deploy"}, "2026-10-07": {"2026-10-07", 40, ""}, "2026-10-09": {"2026-10-09", -50, "Dentist"},
	}
	for i, e := range out {
		if t, ok := typed[e.date]; ok {
			out[i] = t
		}
	}
	return out
}
