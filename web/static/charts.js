// Charts. Two views of the same entries. Ported from design/reference/charts.js.
//   waterfall  how the balance moved: each bar starts where the last one ended
//   calendar   one cell per day, to show rhythm and intensity
// Values are never tooltip only: the Days list is the table view.
(function () {
  const T = TALLY;
  const GUT = 46, XBAND = 22, STEPS = [15, 30, 60, 120, 240, 300, 600, 1200, 2400, 4800];
  const f = (n) => n.toFixed(1);
  const hours = (v) => (v ? T.fmt(v) : '0');

  // Round axis bounds and a step that gives at most `max` intervals.
  function axis(lo, hi, max) {
    lo = Math.min(0, lo); hi = Math.max(0, hi);
    if (hi === lo) hi = 60;
    const step = STEPS.find((s) => (hi - lo) / s <= max) || 9600;
    return { step, lo: Math.floor(lo / step) * step, hi: Math.ceil(hi / step) * step };
  }

  // Days in range, and the same days grouped so there are never more than about 32 bars.
  function prep(span) {
    const days = T.daily(span.from).filter((p) => p.date <= span.to), n = days.length;
    if (!n) return null;
    // A period that crosses into another year names the year, or the months could be any year's.
    const years = span.from.slice(0, 4) !== span.to.slice(0, 4);
    const first = T.get(days[0].date), before = days[0].total - (first ? first.min : 0);
    const unit = n <= 31 ? 'day' : Math.ceil(n / 7) <= 32 ? 'week' : 'month';
    const monday = (s) => { const d = T.parse(s); return T.iso(T.addDays(d, -((d.getDay() + 6) % 7))); };
    const key = (s) => (unit === 'day' ? s : unit === 'week' ? monday(s) : s.slice(0, 7));
    const bins = [];
    days.forEach((p, i) => {
      let b = bins[bins.length - 1];
      if (!b || b.k !== key(p.date)) bins.push(b = { k: key(p.date), a: i, b: i, more: 0, less: 0, entries: [] });
      b.b = i;
      b.end = p.total;
      const e = T.get(p.date);
      if (e) { if (e.min > 0) b.more += e.min; else b.less += e.min; b.entries.push(e); }
    });
    bins.forEach((b, i) => {
      const a = days[b.a].date, z = days[b.b].date;
      b.start = i ? bins[i - 1].end : before;
      b.net = b.more + b.less;
      const y = (d) => (years ? ' ' + d.slice(0, 4) : '');
      b.label = unit === 'day' ? (a === T.today ? 'Today' : T.label(a) + y(a)) : unit === 'week' ? 'Week ' + T.week(T.parse(a)) : T.parse(a).toLocaleString('en', { month: 'long' }) + y(a);
      b.sub = unit === 'day' ? '' : a === z ? T.label(a) + y(a) : `${T.label(a).slice(4)}${y(a)} to ${T.label(z).slice(4)}${y(z)}`;
    });
    return { days, n, before, unit, bins, years };
  }

  // A bar from y0 to y1, rounded at the data end and square at the start.
  function bar(cx, w, y0, y1, cls, i) {
    const up = y1 <= y0, h = Math.max(2, Math.abs(y1 - y0)), r = Math.min(4, w / 2, h), x = cx - w / 2, e = up ? y0 - h : y0 + h, q = up ? e + r : e - r;
    return `<path class="${cls}" data-b="${i}" d="M${f(x)} ${f(y0)}V${f(q)}Q${f(x)} ${f(e)} ${f(x + r)} ${f(e)}H${f(x + w - r)}Q${f(x + w)} ${f(e)} ${f(x + w)} ${f(q)}V${f(y0)}Z"/>`;
  }

  // Shared readout and pointer handling. Values lead, labels follow, text stays in text colour.
  function mount(el, marks, opt) {
    const svg = el.querySelector('svg'), band = svg.querySelector('.band'), pt = svg.querySelector('.pt');
    let cur = -1;
    function show(i) {
      if (i === cur) return;
      cur = i;
      svg.querySelectorAll('.hit').forEach((n) => n.classList.remove('hit'));
      el.classList.toggle('dim', i >= 0 && !!opt.dim);
      let box = el.querySelector('.tip');
      if (i < 0) { if (box) box.classList.remove('on'); if (band) band.classList.remove('on'); if (pt) pt.classList.remove('on'); return; }
      const m = marks[i];
      svg.querySelectorAll(`[data-b="${i}"]`).forEach((n) => n.classList.add('hit'));
      if (band) { band.setAttribute('x', f(m.x0)); band.setAttribute('width', f(Math.max(2, m.x1 - m.x0))); band.classList.add('on'); }
      if (pt) { pt.setAttribute('cx', f((m.x0 + m.x1) / 2)); pt.setAttribute('cy', f(m.py)); pt.classList.add('on'); }
      if (!box) box = el.appendChild(Object.assign(document.createElement('div'), { className: 'tip' }));
      box.textContent = '';
      const add = (cls, text) => { const d = document.createElement('div'); d.className = cls; d.textContent = text; box.appendChild(d); };
      const row = (k, label, value) => {
        const r = document.createElement('div'), sw = document.createElement('i'), s = document.createElement('span'), b = document.createElement('b');
        r.className = 'r'; sw.className = 'k ' + k; s.textContent = label; b.textContent = value;
        r.append(sw, s, b);
        box.appendChild(r);
      };
      opt.fill(m, add, row);
      const at = opt.place(m, box.offsetWidth, box.offsetHeight);
      box.style.left = at.left + 'px';
      box.style.top = at.top + 'px';
      box.classList.add('on');
    }
    el.onpointerdown = el.onpointermove = (e) => { const r = el.getBoundingClientRect(); show(opt.find(e.clientX - r.left, e.clientY - r.top)); };
    el.onpointerleave = (e) => { if (e.pointerType === 'mouse') show(-1); };
    el.onblur = () => show(-1);
    // A chart that lets its marks be picked does so on click, and on Enter for the one shown.
    el.onclick = opt.pick ? (e) => { const r = el.getBoundingClientRect(), i = opt.find(e.clientX - r.left, e.clientY - r.top); if (i >= 0) opt.pick(marks[i]); } : null;
    el.onkeydown = (e) => {
      const by = { ArrowLeft: -1, ArrowRight: 1 }[e.key];
      if (e.key === 'Escape') show(-1);
      if (e.key === 'Enter' && opt.pick && cur >= 0) { e.preventDefault(); opt.pick(marks[cur]); return; }
      if (!by || !marks.length) return;
      e.preventDefault();
      show(Math.max(0, Math.min(marks.length - 1, (cur < 0 ? marks.length - 1 - by : cur) + by)));
    };
  }

  function waterfall(el, span) {
    const W = el.clientWidth, H = el.clientHeight;
    const D = W && H && prep(span);
    if (!D) return;
    const PW = W - GUT, top = 28, y1 = H - XBAND, xs = (i) => (i / D.n) * PW;
    const vals = D.bins.map((b) => b.end).concat(D.before), ax = axis(Math.min(...vals), Math.max(...vals), 6), Y = (v) => top + ((ax.hi - v) / (ax.hi - ax.lo)) * (y1 - top);
    let grid = '', labels = '', conn = `M0 ${f(Y(D.before))}`, cols = '';
    for (let v = ax.lo; v <= ax.hi; v += ax.step) grid += `<line class="${v ? 'g' : 'z'}" x1="0" x2="${PW}" y1="${f(Y(v))}" y2="${f(Y(v))}"/><text x="${W}" y="${f(Y(v) + 4)}" text-anchor="end">${hours(v)}</text>`;
    // Labels never crowd. Across years a January is named by its year and placed first, then
    // every other label that keeps clear of those already placed.
    const marks = [], gap = D.unit === 'day' ? 40 : 30;
    D.days.forEach((p, i) => {
      const d = T.parse(p.date), hit = D.unit === 'day' ? d.getDay() === 1 : d.getDate() === 1, x = xs(i);
      if (!hit || x >= PW - 22) return;
      const year = D.unit !== 'day' && D.years && d.getMonth() === 0;
      marks.push({ x, year, text: D.unit === 'day' ? `${d.getDate()} ${T.MONTHS[d.getMonth()]}` : year ? d.getFullYear() : T.MONTHS[d.getMonth()] });
    });
    const placed = [];
    [true, false].forEach((first) => marks.forEach((m) => {
      if (m.year === first && placed.every((q) => Math.abs(q.x - m.x) >= gap)) placed.push(m);
    }));
    placed.forEach((m) => { labels += `<text x="${f(m.x)}" y="${H - 5}">${m.text}</text>`; });
    D.bins.forEach((b, i) => {
      const x0 = xs(b.a), x1 = xs(b.b + 1), w = Math.max(2, Math.min(24, x1 - x0 - 2)), cx = (x0 + x1) / 2;
      b.x0 = x0; b.x1 = x1; b.py = Y(b.end);
      conn += `H${f(cx)}V${f(Y(b.end))}`;
      if (b.net) cols += bar(cx, w, Y(b.start), Y(b.end), b.net > 0 ? 'more' : 'less', i);
    });
    const last = D.bins[D.bins.length - 1].end, lx = PW - 112;
    el.innerHTML = `<svg width="${W}" height="${H}" viewBox="0 0 ${W} ${H}" role="img" aria-label="Balance over time, one bar per ${D.unit} from the last level to the new one">
      <rect class="band" y="${top}" height="${y1 - top}"/>${grid}${labels}
      <text class="cap" x="0" y="12">Net change per ${D.unit}</text>
      <rect class="more" x="${lx}" y="3" width="10" height="10" rx="2"/><text x="${lx + 15}" y="12">More</text><rect class="less" x="${lx + 56}" y="3" width="10" height="10" rx="2"/><text x="${lx + 71}" y="12">Less</text>
      <path class="conn" d="${conn}H${PW}"/>${cols}
      <circle class="end" cx="${PW}" cy="${f(Y(last))}" r="4.5"/><text class="endl" x="${PW - 9}" y="${f(Y(last) - 9)}" text-anchor="end">${T.fmt(last)}</text>
      <circle class="pt" r="4.5"/>
    </svg>`;
    mount(el, D.bins, {
      dim: true,
      find: (x) => D.bins.findIndex((m) => x >= m.x0 && x < m.x1),
      place: (m, tw) => ({ left: Math.max(0, m.x1 + 10 + tw <= W ? m.x1 + 10 : m.x0 - 10 - tw), top: 4 }),
      fill(m, add, row) {
        add('t', m.label);
        if (m.sub) add('s', m.sub);
        row('line', 'Balance', T.fmt(m.end));
        if (m.net) row(m.net > 0 ? 'more' : 'less', 'Change', T.fmt(m.net)); else add('s', 'No change');
        if (m.more && m.less) add('s', `${T.fmt(m.more)} and ${T.fmt(m.less)}`);
        if (D.unit === 'day' && m.entries[0] && m.entries[0].note) add('s', m.entries[0].note);
      },
    });
  }

  // One cell per day. Grey is a normal day, with steps at 30m, 1h and 2h toward each colour.
  // Weekends are always there, since weekend work gets logged too, and set apart: a gap above
  // them, and dimmer cells when nothing was logged. Weeks wrap into bands so a cell stays big
  // enough to read and to tap. The box sizes itself.
  // until, when later than the span's end, adds the rest of the period as outlines: days to come.
  function calendar(el, span, until) {
    const W = el.clientWidth;
    const D = W && prep(span);
    if (!D) return;
    const later = [];
    for (let d = T.addDays(T.parse(span.to), 1); until && T.iso(d) <= until; d = T.addDays(d, 1)) later.push(T.iso(d));
    const lead = (T.parse(D.days[0].date).getDay() + 6) % 7, cols = Math.ceil((D.n + later.length + lead) / 7);
    const left = 22, head = 20, gap = 6, bands = Math.max(1, Math.ceil((cols * 22) / (W - left))), per = Math.ceil(cols / bands);
    const s = Math.max(8, Math.min(28, Math.floor((W - left) / per) - 2)), pitch = s + 2, bandH = head + 7 * pitch + gap + 12;
    const rowY = (r) => r * pitch + (r >= 5 ? gap : 0);
    const level = (min) => { const a = Math.abs(min); return (min > 0 ? 'm' : 'l') + (a <= 30 ? 1 : a <= 60 ? 2 : a <= 120 ? 3 : 4); };
    let cells = '', heads = '', sides = '';
    const marks = [];
    D.days.forEach((p, i) => {
      const c = Math.floor((i + lead) / 7), r = (i + lead) % 7, band = Math.floor(c / per), x = left + (c % per) * pitch, d = T.parse(p.date), e = T.get(p.date);
      if (d.getDate() === 1 || i === 0) heads += `<text x="${x}" y="${band * bandH + 12}">${D.years && d.getMonth() === 0 && d.getDate() === 1 ? d.getFullYear() : T.MONTHS[d.getMonth()]}</text>`;
      const y = band * bandH + head + rowY(r), off = r >= 5;
      cells += `<rect class="${e ? level(e.min) : off ? 'off' : 'none'}${p.date === T.today ? ' today' : ''}" data-b="${marks.length}" data-d="${p.date}" x="${x}" y="${y}" width="${s}" height="${s}" rx="${Math.min(4, s / 5)}"/>`;
      marks.push({ x0: x, x1: x + s, y0: y, top: band * bandH, date: p.date, e, end: p.total, off });
    });
    later.forEach((date, j) => {
      const i = D.n + j, c = Math.floor((i + lead) / 7), r = (i + lead) % 7, band = Math.floor(c / per), x = left + (c % per) * pitch, d = T.parse(date);
      if (d.getDate() === 1) heads += `<text x="${x}" y="${band * bandH + 12}">${T.MONTHS[d.getMonth()]}</text>`;
      cells += `<rect class="later" x="${f(x + 0.5)}" y="${f(band * bandH + head + rowY(r) + 0.5)}" width="${s - 1}" height="${s - 1}" rx="${Math.min(4, s / 5)}"/>`;
    });
    for (let b = 0; b < bands; b++) sides += ['M', 'T', 'W', 'T', 'F', 'S', 'S'].map((d, r) => `<text x="0" y="${f(b * bandH + head + rowY(r) + s / 2 + 4)}">${d}</text>`).join('');
    const ly = bands * bandH + 14, sw = Math.min(16, s), key = ['l4', 'l3', 'l2', 'l1', 'none', 'm1', 'm2', 'm3', 'm4'], H = ly + 26;
    el.style.height = H + 'px';
    el.innerHTML = `<svg class="cal" width="${W}" height="${H}" viewBox="0 0 ${W} ${H}" role="img" aria-label="One cell per day, weekends below the weekdays, coloured by how much more or less was worked">
      ${heads}${sides}${cells}
      <text x="${left}" y="${ly}">Less</text>${key.map((k, i) => `<rect class="${k}" x="${left + 34 + i * (sw + 2)}" y="${ly - sw + 3}" width="${sw}" height="${sw}" rx="2"/>`).join('')}<text x="${left + 40 + key.length * (sw + 2)}" y="${ly}">More</text>
      <text class="steps" x="${left}" y="${ly + 18}">Steps at 30m, 1h, 2h</text><rect class="off wk" y="${ly + 18 - 9}" width="10" height="10" rx="2"/><text class="wk" y="${ly + 18}">Weekend</text>
    </svg>`;
    // The weekend key follows the steps line, wherever the font ends it.
    let after = left + 130;
    try { after = left + el.querySelector('.steps').getComputedTextLength() + 16; } catch (_) { /* keep the estimate */ }
    const [wkKey, wkText] = el.querySelectorAll('.wk');
    wkKey.setAttribute('x', f(after));
    wkText.setAttribute('x', f(after + 15));
    mount(el, marks, {
      pick: Charts.onPick && ((m) => Charts.onPick(m.date)),
      find: (x, y) => marks.findIndex((m) => x >= m.x0 - 1 && x < m.x1 + 1 && y >= m.y0 - 1 && y < m.y0 + pitch),
      // Above the band the cell is in, clear of the days next to it.
      place: (m, tw, th) => ({ left: Math.max(0, Math.min(W - tw, (m.x0 + m.x1) / 2 - tw / 2)), top: m.top - th + 14 }),
      fill(m, add, row) {
        add('t', m.date === T.today ? 'Today' : T.label(m.date) + (D.years ? ' ' + m.date.slice(0, 4) : ''));
        if (m.e) row(m.e.min > 0 ? 'more' : 'less', m.e.min > 0 ? 'More' : 'Less', T.fmt(m.e.min)); else add('s', m.off ? 'Weekend' : 'Normal day');
        if (m.e && m.e.note) add('s', m.e.note);
        row('line', 'Balance', T.fmt(m.end));
      },
    });
  }

  // Charts.onPick, when set, is called with the date of a calendar day that was clicked.
  window.Charts = { waterfall, calendar, onPick: null };
})();
