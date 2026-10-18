package tally

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"time"

	"tally/internal/store"
)

// Service applies tally's rules on top of the store.
type Service struct {
	Store *store.Store
	// Loc decides when a day starts and ends. Today is today in this zone.
	Loc *time.Location
	// Now is replaceable in tests.
	Now func() time.Time
}

func New(s *store.Store, loc *time.Location) *Service {
	return &Service{Store: s, Loc: loc, Now: time.Now}
}

// Today is the current day in the service's time zone.
func (s *Service) Today() string { return s.Now().In(s.Loc).Format(DateLayout) }

// checkDay accepts today and past days only.
func (s *Service) checkDay(date string) error {
	if _, err := ParseDate(date); err != nil {
		return invalid("%s", err.Error())
	}
	if date > s.Today() {
		return invalid("%s is in the future, tally only takes today and past days", date)
	}
	if date < earliest {
		return invalid("%s is too far back", date)
	}
	return nil
}

// Change is what happened to one day.
type Change struct {
	Date   string       `json:"date"`
	Before *store.Entry `json:"before"`
	After  *store.Entry `json:"after"`
}

// Add puts minutes onto a day. A day that has an entry adds to it. A new note replaces the old
// one, an empty note keeps it. A day that reaches 0 is removed.
func (s *Service) Add(ctx context.Context, date string, minutes int, note string) (Change, error) {
	if err := s.checkDay(date); err != nil {
		return Change{}, err
	}
	if minutes == 0 {
		return Change{}, invalid("minutes must not be 0")
	}
	if minutes > MaxAdd || minutes < -MaxAdd {
		return Change{}, invalid("at most %s at a time", Format(MaxAdd)[1:])
	}
	note, err := CleanNote(note)
	if err != nil {
		return Change{}, err
	}
	before, after, err := s.Store.Update(ctx, date, func(old *store.Entry) (*store.Entry, error) {
		e := store.Entry{Minutes: minutes, Note: note}
		if old != nil {
			e.Minutes += old.Minutes
			if note == "" {
				e.Note = old.Note
			}
		}
		if e.Minutes > MaxEntry || e.Minutes < -MaxEntry {
			return nil, invalid("a day holds at most %s", Format(MaxEntry)[1:])
		}
		return &e, nil
	})
	return Change{Date: date, Before: before, After: after}, err
}

// Set makes a day hold exactly minutes and note. 0 removes it. Undo uses this to put a day back.
func (s *Service) Set(ctx context.Context, date string, minutes int, note string) (Change, error) {
	if err := s.checkDay(date); err != nil {
		return Change{}, err
	}
	if minutes > MaxEntry || minutes < -MaxEntry {
		return Change{}, invalid("a day holds at most %s", Format(MaxEntry)[1:])
	}
	note, err := CleanNote(note)
	if err != nil {
		return Change{}, err
	}
	before, after, err := s.Store.Update(ctx, date, func(*store.Entry) (*store.Entry, error) {
		if minutes == 0 {
			return nil, nil
		}
		return &store.Entry{Minutes: minutes, Note: note}, nil
	})
	return Change{Date: date, Before: before, After: after}, err
}

// ErrConflict means a day changed in between, so an undo would throw away someone else's change.
var ErrConflict = errors.New("the day changed since")

// Restore puts a day back to what it held before a change, but only while the day still holds
// what that change left behind. Undo uses it, so it never overwrites a change made in between
// from another device or by an agent. A nil entry means no entry.
func (s *Service) Restore(ctx context.Context, date string, to, expect *store.Entry) (Change, error) {
	if err := s.checkDay(date); err != nil {
		return Change{}, err
	}
	if to != nil {
		if to.Minutes > MaxEntry || to.Minutes < -MaxEntry {
			return Change{}, invalid("a day holds at most %s", Format(MaxEntry)[1:])
		}
		note, err := CleanNote(to.Note)
		if err != nil {
			return Change{}, err
		}
		to = &store.Entry{Minutes: to.Minutes, Note: note}
	}
	before, after, err := s.Store.Update(ctx, date, func(old *store.Entry) (*store.Entry, error) {
		if !sameEntry(old, expect) {
			return nil, ErrConflict
		}
		return to, nil
	})
	return Change{Date: date, Before: before, After: after}, err
}

func sameEntry(a, b *store.Entry) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Minutes == b.Minutes && a.Note == b.Note
}

// Delete removes a day. Deleting a day with no entry is not an error.
func (s *Service) Delete(ctx context.Context, date string) (Change, error) {
	if _, err := ParseDate(date); err != nil {
		return Change{}, invalid("%s", err.Error())
	}
	before, _, err := s.Store.Update(ctx, date, func(*store.Entry) (*store.Entry, error) { return nil, nil })
	return Change{Date: date, Before: before}, err
}

// Entries returns every entry up to today, oldest first.
func (s *Service) Entries(ctx context.Context) ([]store.Entry, error) {
	return s.Store.Entries(ctx, "", s.Today())
}

// Summary is the balance now and how it changed this week, month, quarter and year.
type Summary struct {
	Today      string `json:"today"`
	Balance    Amount `json:"balance"`
	Week       Period `json:"week"`
	Month      Period `json:"month"`
	Quarter    Period `json:"quarter"`
	Year       Period `json:"year"`
	DaysLogged int    `json:"days_logged"`
	FirstDay   string `json:"first_day,omitempty"`
	LastDay    string `json:"last_day,omitempty"`
}

// Period is the net change from From up to today.
type Period struct {
	From string `json:"from"`
	Amount
}

func (s *Service) Summary(ctx context.Context) (Summary, error) {
	today := s.Today()
	all, err := s.Store.Entries(ctx, "", today)
	if err != nil {
		return Summary{}, err
	}
	sum := Summary{Today: today, DaysLogged: len(all)}
	if len(all) > 0 {
		sum.FirstDay, sum.LastDay = all[0].Date, all[len(all)-1].Date
	}
	total := 0
	for _, e := range all {
		total += e.Minutes
	}
	sum.Balance = amount(total)
	since := func(unit string) Period {
		from, n := StartOf(unit, today), 0
		for _, e := range all {
			if e.Date >= from {
				n += e.Minutes
			}
		}
		return Period{From: from, Amount: amount(n)}
	}
	sum.Week, sum.Month, sum.Quarter, sum.Year = since("week"), since("month"), since("quarter"), since("year")
	return sum, nil
}

// Day is one logged day with the balance at the end of it.
type Day struct {
	Date    string `json:"date"`
	Weekday string `json:"weekday"`
	Amount
	Note    string `json:"note,omitempty"`
	Balance Amount `json:"balance"`
}

// DayList is the logged days in a range and the net change over it.
type DayList struct {
	From       string `json:"from"`
	To         string `json:"to"`
	Change     Amount `json:"change"`
	DaysLogged int    `json:"days_logged"`
	Days       []Day  `json:"days"`
}

// Days lists logged days from from to to, oldest first, each with the running balance.
// The range defaults to the last 30 days.
func (s *Service) Days(ctx context.Context, from, to string) (DayList, error) {
	from, to, err := s.span(from, to, 30)
	if err != nil {
		return DayList{}, err
	}
	all, err := s.Store.Entries(ctx, "", to)
	if err != nil {
		return DayList{}, err
	}
	out := DayList{From: from, To: to, Days: []Day{}}
	total, change := 0, 0
	for _, e := range all {
		total += e.Minutes
		if e.Date < from {
			continue
		}
		change += e.Minutes
		out.Days = append(out.Days, Day{Date: e.Date, Weekday: Weekday(e.Date), Amount: amount(e.Minutes), Note: e.Note, Balance: amount(total)})
	}
	out.Change, out.DaysLogged = amount(change), len(out.Days)
	return out, nil
}

// Week is the change over one ISO week, Monday to Sunday.
type Week struct {
	Week       string `json:"week"`
	From       string `json:"from"`
	To         string `json:"to"`
	Change     Amount `json:"change"`
	More       Amount `json:"more"`
	Less       Amount `json:"less"`
	Balance    Amount `json:"balance"`
	DaysLogged int    `json:"days_logged"`
}

// TrendList is the balance week by week.
type TrendList struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Weeks []Week `json:"weeks"`
}

// Trend gives the balance at the end of every week from from to to, by default the last 12
// weeks. Weeks before the first entry are left out, they would all read 0.
func (s *Service) Trend(ctx context.Context, from, to string) (TrendList, error) {
	open := from == ""
	from, to, err := s.span(from, to, 1)
	if err != nil {
		return TrendList{}, err
	}
	if open {
		from = addDays(Monday(to), -7*11)
	}
	all, err := s.Store.Entries(ctx, "", to)
	if err != nil {
		return TrendList{}, err
	}
	out := TrendList{From: Monday(from), To: to, Weeks: []Week{}}
	if len(all) == 0 {
		return out, nil
	}
	if all[0].Date > from {
		from = all[0].Date
		out.From = Monday(from)
	}
	before, i := 0, 0
	for ; i < len(all) && all[i].Date < out.From; i++ {
		before += all[i].Minutes
	}
	for mon := out.From; mon <= to; mon = addDays(mon, 7) {
		sun := addDays(mon, 6)
		w := Week{Week: ISOWeek(mon), From: mon, To: sun}
		more, less := 0, 0
		for ; i < len(all) && all[i].Date <= sun; i++ {
			if m := all[i].Minutes; m > 0 {
				more += m
			} else {
				less += m
			}
			w.DaysLogged++
		}
		before += more + less
		w.Change, w.More, w.Less, w.Balance = amount(more+less), amount(more), amount(less), amount(before)
		if w.To > to {
			w.To = to
		}
		out.Weeks = append(out.Weeks, w)
	}
	return out, nil
}

// span checks a range and fills in an open end: to defaults to today, from to days before to.
func (s *Service) span(from, to string, days int) (string, string, error) {
	for _, d := range []string{from, to} {
		if _, err := ParseDate(d); d != "" && err != nil {
			return "", "", invalid("%s", err.Error())
		}
	}
	if today := s.Today(); to == "" || to > today {
		to = today
	}
	if from == "" {
		from = addDays(to, 1-days)
	}
	if from > to {
		return "", "", invalid("from %s is after to %s", from, to)
	}
	return from, to, nil
}

// Write access for agents is off until the user turns it on.
const settingWrite = "agents_write"

func (s *Service) AgentsCanWrite(ctx context.Context) (bool, error) {
	v, _, err := s.Store.Setting(ctx, settingWrite)
	return v == "1", err
}

func (s *Service) SetAgentsCanWrite(ctx context.Context, on bool) error {
	v := "0"
	if on {
		v = "1"
	}
	return s.Store.SetSetting(ctx, settingWrite, v)
}

// Agents prove who they are with one token per install, made on first use.
const settingToken = "mcp_token"

func newToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return "tly_" + base64.RawURLEncoding.EncodeToString(b)
}

// MCPToken returns the token agents send, making it if there is none yet.
func (s *Service) MCPToken(ctx context.Context) (string, error) {
	t, ok, err := s.Store.Setting(ctx, settingToken)
	if err != nil || ok {
		return t, err
	}
	t = newToken()
	return t, s.Store.SetSetting(ctx, settingToken, t)
}

// RotateMCPToken replaces the token. Agents using the old one stop working at once.
func (s *Service) RotateMCPToken(ctx context.Context) (string, error) {
	t := newToken()
	if err := s.Store.SetSetting(ctx, settingToken, t); err != nil {
		return "", err
	}
	return t, s.Store.DeleteAllMCPSessions(ctx)
}
