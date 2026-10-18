// Entries, date helpers and the server. Ported from design/reference/data.js.
// The server renders the first state into the page, so the app draws without waiting on a request.
// Saves change the local copy at once and go to the server in order behind it.
(function () {
  const pad = (n) => String(n).padStart(2, '0');
  const iso = (d) => `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
  const parse = (s) => { const [y, m, d] = s.split('-').map(Number); return new Date(y, m - 1, d); };
  const addDays = (d, n) => { const c = new Date(d); c.setDate(c.getDate() + n); return c; };
  const DAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
  const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec'];

  const boot = JSON.parse(document.getElementById('state').textContent);
  let entries = [], byDate = new Map();
  function load(list) {
    entries = list.map((e) => ({ date: e.date, min: e.minutes, note: e.note || '' })).sort((a, b) => (a.date < b.date ? -1 : 1));
    byDate = new Map(entries.map((e) => [e.date, e]));
  }
  load(boot.entries);

  const sum = (list) => list.reduce((t, e) => t + e.min, 0);
  let tweens = 0;

  const T = {
    today: boot.today, agents: boot.agents, DAYS, MONTHS, iso, parse, addDays,
    get entries() { return entries; },
    // The first logged day. A period never starts before it.
    get start() { return entries.length && entries[0].date < T.today ? entries[0].date : T.today; },

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

    get(date) { return byDate.get(date); },
    set(date, min, note = '') {
      const i = entries.findIndex((e) => e.date === date);
      if (i >= 0) entries.splice(i, 1);
      byDate.delete(date);
      if (min) {
        const e = { date, min, note };
        entries.push(e);
        entries.sort((a, b) => (a.date < b.date ? -1 : 1));
        byDate.set(date, e);
      }
    },
    balance(upTo = T.today) { return sum(entries.filter((e) => e.date <= upTo)); },
    // Net change from a date up to today.
    net(from) { return sum(entries.filter((e) => e.date >= from && e.date <= T.today)); },
    // First day of this week, month, quarter or year. Weeks start on Monday.
    startOf(unit) {
      const today = parse(T.today), y = today.getFullYear(), m = today.getMonth();
      if (unit === 'week') return iso(addDays(today, -((today.getDay() + 6) % 7)));
      if (unit === 'month') return iso(new Date(y, m, 1));
      if (unit === 'quarter') return iso(new Date(y, m - (m % 3), 1));
      return iso(new Date(y, 0, 1));
    },

    // Running balance for every day from `from` to today.
    daily(from = T.start) {
      const out = [], end = parse(T.today);
      let total = sum(entries.filter((e) => e.date < from));
      for (let d = parse(from); d <= end; d = addDays(d, 1)) {
        const key = iso(d), e = byDate.get(key);
        if (e) total += e.min;
        out.push({ date: key, total });
      }
      return out;
    },
    // Count a number from a to b in quarter hours. A new count stops the one before it.
    tween(a, b, paint) {
      const run = ++tweens;
      if (a === b || matchMedia('(prefers-reduced-motion: reduce)').matches) return paint(b);
      const t0 = performance.now(), dur = 420;
      (function tick(now) {
        if (run !== tweens) return;
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

  // Today follows the server's time zone, so the app and agents agree on it. The browser works
  // it out from its own clock, corrected by how far that clock is from the server's, so today
  // moves on at midnight and the moment a phone wakes, without waiting for a request.
  let skew = boot.now - Date.now();
  const dayIn = (() => {
    try {
      const f = new Intl.DateTimeFormat('en-CA', { timeZone: boot.zone, year: 'numeric', month: '2-digit', day: '2-digit' });
      return (ms) => { const p = Object.fromEntries(f.formatToParts(new Date(ms)).map((x) => [x.type, x.value])); return `${p.year}-${p.month}-${p.day}`; };
    } catch (_) { return null; }
  })();
  // The server's clock, and the day a moment falls on in the server's zone.
  T.now = () => Date.now() + skew;
  T.dayOf = (ms) => (dayIn ? dayIn(ms) : iso(new Date(ms)));
  // Returns true when the day changed.
  T.tick = () => {
    if (!dayIn) return false;
    const now = dayIn(T.now());
    if (now === T.today) return false;
    T.today = now;
    return true;
  };

  // Server. Requests run one at a time in the order they were made, so a save and its undo
  // can never cross. Once the queue is empty the app pulls the server's state to settle on.
  // `confirmed` is what the server has said, so a failed change can be rolled back even when
  // the server cannot be reached to ask.
  let queue = Promise.resolve(), pending = 0, failures = [], epoch = 0, seq = 0, receipts = [], signedOut = false;
  const copy = (list) => list.map((e) => ({ ...e }));
  let confirmed = { entries: copy(entries), write: T.agents.write };

  // A request that hangs, as one can when a phone moves from wifi to mobile data, gives up after
  // 15 seconds, so it fails like any other instead of holding up every change behind it.
  async function api(method, path, body) {
    const stop = new AbortController(), timer = setTimeout(() => stop.abort(), 15000);
    try {
      const res = await fetch(path, {
        method,
        headers: body ? { 'Content-Type': 'application/json' } : {},
        body: body ? JSON.stringify(body) : undefined,
        credentials: 'same-origin',
        cache: 'no-store',
        signal: stop.signal,
      });
      // Signed out, for example after a password change. The page shows sign in, and says so
      // when a change was lost with it. The first answer decides, later ones would only undo that.
      if (res.status === 401) {
        if (!signedOut) location.replace(method !== 'GET' || pending ? '/?unsaved' : '/');
        signedOut = true;
        throw new Error('signed out');
      }
      if (!res.ok) throw new Error((await res.json().catch(() => ({}))).error || res.statusText);
      // A 200 that is not JSON, like a captive portal's page, is a failure too.
      return await res.json();
    } finally {
      clearTimeout(timer);
    }
  }

  // What a successful change tells us about the server.
  function confirm(res) {
    if (res && res.date) {
      confirmed.entries = confirmed.entries.filter((e) => e.date !== res.date);
      if (res.after) confirmed.entries.push({ date: res.date, min: res.after.minutes, note: res.after.note || '' });
    }
    if (res && typeof res.on === 'boolean') confirmed.write = res.on;
  }

  // Send a change and return the server's answer. body may be a function, run when the change's
  // turn comes, so it can use the answer to an earlier change. A function returning null skips
  // the change. change describes it for when it fails: { id, fail, date, target } where fail is
  // the toast and target the entry the day should hold after it (null for none), or { id, fail,
  // held } for other changes. T.onSettled runs once the queue is empty, with the changes that
  // failed and which parts of the state changed.
  T.send = (method, path, body, change = {}) => {
    pending++;
    epoch++;
    const n = ++seq;
    const run = queue.then(async () => {
      const b = typeof body === 'function' ? await body() : body;
      if (b === null) return null;
      const res = await api(method, path, b);
      confirm(res);
      if (res && res.date) receipts.push({ n, date: res.date, before: res.before });
      return res;
    });
    // The queue itself never fails, or one failure would stop every change after it.
    queue = run
      .catch((err) => { failures.push({ ...change, n }); console.error(err); })
      .finally(() => {
        if (--pending) return;
        const failed = failures;
        failures = [];
        settle(failed).catch((err) => console.error(err));
      });
    return run;
  };

  const sameEntry = (e, target) => (e && target ? e.minutes === target.minutes && (e.note || '') === target.note : !e && !target);
  // A request can go through with its answer lost on the way back. Such a change did not fail.
  // The next change to the same day that did get an answer says what the day held before it,
  // which is what the lost change left. Without one, the state just pulled tells.
  function held(c, pulled) {
    if (c.held) return pulled && c.held();
    if (!c.date) return false;
    const next = receipts.find((r) => r.n > c.n && r.date === c.date);
    if (next) return sameEntry(next.before, c.target);
    const e = T.get(c.date);
    return pulled && sameEntry(e && { minutes: e.min, note: e.note }, c.target);
  }

  async function settle(failed) {
    let changed = await T.pull();
    if (!pending) {
      failed = failed.filter((c) => !held(c, !!changed));
      receipts = [];
    }
    // Newer changes went out meanwhile. They settle on their own, this only reports a failure.
    if (pending) changed = {};
    else if (!changed && failed.length) {
      // The server cannot be reached either. Show what it last confirmed.
      load(confirmed.entries.map((e) => ({ date: e.date, minutes: e.min, note: e.note })));
      T.agents.write = confirmed.write;
      changed = { entries: true, agents: true };
    }
    if (T.onSettled) T.onSettled(failed, changed || {});
  }

  // Fetch the state and adopt it, unless a change was made while it was on its way. Resolves to
  // which parts changed ({entries, agents}, entries covering today too), or null when the state
  // could not be fetched or was dropped.
  T.pull = async () => {
    const seen = epoch;
    let s;
    try { s = await api('GET', '/api/state'); } catch (_) { return null; }
    const ok = s && typeof s.today === 'string' && Array.isArray(s.entries) && s.agents && typeof s.agents === 'object' && Number.isFinite(s.now);
    if (!ok || seen !== epoch || pending) return null;
    skew = s.now - Date.now();
    const before = { entries: JSON.stringify([T.today, entries]), agents: JSON.stringify(T.agents) };
    T.today = s.today;
    T.agents = s.agents;
    load(s.entries);
    confirmed = { entries: copy(entries), write: s.agents.write };
    return { entries: before.entries !== JSON.stringify([T.today, entries]), agents: before.agents !== JSON.stringify(T.agents) };
  };
  T.busy = () => pending > 0;

  window.TALLY = T;
})();
