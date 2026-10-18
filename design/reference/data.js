// Sample data and date helpers for the reference. In memory, resets on reload.
// Today is fixed to 12 Oct 2026 so the charts always look the same.
(function () {
  let seed = 11;
  const rnd = () => (seed = (seed * 1664525 + 1013904223) >>> 0) / 4294967296;
  const pad = (n) => String(n).padStart(2, '0');
  const iso = (d) => `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
  const parse = (s) => { const [y, m, d] = s.split('-').map(Number); return new Date(y, m - 1, d); };
  const addDays = (d, n) => { const c = new Date(d); c.setDate(c.getDate() + n); return c; };
  const DAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
  const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];
  const TODAY = new Date(2026, 9, 12), START = new Date(2026, 3, 6);

  // Minutes against a normal day, always a multiple of 15.
  const fixed = {
    '2026-08-14': [-240, 'Comp'], '2026-08-28': [-480, 'Day off'],
    '2026-10-01': [45, ''], '2026-10-02': [-60, 'School'], '2026-10-05': [90, 'Deploy'],
    '2026-10-07': [30, ''], '2026-10-09': [-45, 'Dentist'],
  };
  const overNotes = ['Release', 'Deploy', 'On call', 'Workshop', 'Travel', 'Review', '', '', '', ''];
  const underNotes = ['Dentist', 'School', 'Errand', '', '', ''];
  // month: [chance of an entry, share that are over, largest size in quarter hours]
  const phase = { 3: [.4, .35, 6], 4: [.45, .65, 6], 5: [.7, .88, 12], 6: [.3, .6, 5], 7: [.35, .45, 6], 8: [.45, .62, 7], 9: [0, 0, 0] };
  const entries = [];
  for (let d = new Date(START); d < TODAY; d = addDays(d, 1)) {
    if (d.getDay() === 0 || d.getDay() === 6) continue;
    const key = iso(d);
    if (fixed[key]) { entries.push({ date: key, min: fixed[key][0], note: fixed[key][1] }); continue; }
    if (d >= new Date(2026, 6, 13) && d <= new Date(2026, 7, 2)) continue; // summer leave
    const [chance, over, max] = phase[d.getMonth()];
    if (rnd() > chance) continue;
    const sign = rnd() < over ? 1 : -1;
    const q = 1 + Math.floor(Math.pow(rnd(), 1.8) * max);
    const notes = sign > 0 ? overNotes : underNotes;
    entries.push({ date: key, min: sign * q * 15, note: notes[Math.floor(rnd() * notes.length)] });
  }

  const sum = (list) => list.reduce((t, e) => t + e.min, 0);

  const T = {
    today: iso(TODAY), start: iso(START), entries, DAYS, MONTHS, iso, parse, addDays,

    fmt(min, sign = true) {
      const a = Math.abs(min), h = Math.floor(a / 60), m = a % 60;
      const s = !sign ? '' : min > 0 ? '+' : min < 0 ? '−' : '';
      return s + (h && m ? `${h}h ${m}m` : h ? `${h}h` : `${m}m`);
    },
    label(date) { const d = parse(date); return `${DAYS[d.getDay()]} ${d.getDate()} ${MONTHS[d.getMonth()]}`; },
    week(d) {
      const t = new Date(Date.UTC(d.getFullYear(), d.getMonth(), d.getDate()));
      t.setUTCDate(t.getUTCDate() + 4 - (t.getUTCDay() || 7));
      return Math.ceil(((t - Date.UTC(t.getUTCFullYear(), 0, 1)) / 864e5 + 1) / 7);
    },

    get(date) { return entries.find((e) => e.date === date); },
    set(date, min, note = '') {
      const i = entries.findIndex((e) => e.date === date);
      if (i >= 0) entries.splice(i, 1);
      if (min) { entries.push({ date, min, note }); entries.sort((a, b) => (a.date < b.date ? -1 : 1)); }
    },
    balance(upTo = T.today) { return sum(entries.filter((e) => e.date <= upTo)); },
    // Net change from a date up to today.
    net(from) { return sum(entries.filter((e) => e.date >= from && e.date <= T.today)); },
    // First day of this week, month, quarter or year. Weeks start on Monday.
    startOf(unit) {
      const y = TODAY.getFullYear(), m = TODAY.getMonth();
      if (unit === 'week') return iso(addDays(TODAY, -((TODAY.getDay() + 6) % 7)));
      if (unit === 'month') return iso(new Date(y, m, 1));
      if (unit === 'quarter') return iso(new Date(y, m - (m % 3), 1));
      return iso(new Date(y, 0, 1));
    },

    // Running balance for every day from `from` to today.
    daily(from = T.start) {
      const out = [];
      let total = sum(entries.filter((e) => e.date < from));
      for (let d = parse(from); d <= TODAY; d = addDays(d, 1)) {
        const key = iso(d), e = T.get(key);
        if (e) total += e.min;
        out.push({ date: key, total });
      }
      return out;
    },
    // Count a number from a to b in quarter hours.
    tween(a, b, paint) {
      if (a === b || matchMedia('(prefers-reduced-motion: reduce)').matches) return paint(b);
      const t0 = performance.now(), dur = 420;
      (function tick(now) {
        const k = Math.min(1, (now - t0) / dur);
        paint(Math.round((a + (b - a) * k) / 15) * 15);
        if (k < 1) requestAnimationFrame(tick); else paint(b);
      })(t0);
    },
    // Screens are today, balance and agents. The hash picks one.
    route(show) {
      const go = () => {
        const id = (location.hash || '#today').slice(1);
        show(['today', 'balance', 'agents'].includes(id) ? id : 'today');
      };
      addEventListener('hashchange', go);
      go();
    },
  };

  window.TALLY = T;
})();
