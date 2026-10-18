// tally reference behaviour. State lives in memory and resets on reload.
const T = TALLY, $ = (s, r = document) => r.querySelector(s), $$ = (s, r = document) => [...r.querySelectorAll(s)];
const P = {
  plus: '<path d="M5 12h14M12 5v14"/>',
  x: '<path d="M18 6 6 18M6 6l12 12"/>',
  chev: '<path d="m6 9 6 6 6-6"/>',
  chart: '<path d="M3 3v18h18"/><path d="m19 9-5 5-4-4-3 3"/>',
  plug: '<path d="M12 22v-5M9 8V2M15 8V2M18 8v5a4 4 0 0 1-4 4h-4a4 4 0 0 1-4-4V8Z"/>',
};
const icon = (n, s = 22) => `<svg class="i" width="${s}" height="${s}" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${P[n]}</svg>`;
const tone = (m) => (m > 0 ? 'more' : m < 0 ? 'less' : 'even');
$$('[data-i]').forEach((el) => { el.innerHTML = icon(el.dataset.i, +el.dataset.s || 22); });

// A few typed amounts, so the lists show what free form looks like.
T.set('2026-10-05', 80, 'Deploy');
T.set('2026-10-07', 40, '');
T.set('2026-10-09', -50, 'Dentist');

// Add. Any amount, saved onto the picked day. Undo instead of confirm.
let dir = 1, sel = T.today, last = null, toastTimer;
const app = $('#app'), form = $('#form'), h = $('#h'), m = $('#m'), note = $('#note'), day = $('#day'), save = $('#save');

const amount = () => (parseInt(h.value, 10) || 0) * 60 + (parseInt(m.value, 10) || 0);
function setAmount(min) {
  h.value = min >= 60 ? Math.floor(min / 60) : '';
  m.value = min % 60 || '';
}
const row = (e) => `<li><span class="grow"><b>${e.date === T.today ? 'Today' : T.label(e.date)}</b>${e.note ? `<small>${e.note}</small>` : ''}</span><span class="v tn ${tone(e.min)}">${T.fmt(e.min)}</span><button class="del" data-del="${e.date}" aria-label="Delete ${T.label(e.date)}">${icon('x', 18)}</button></li>`;

function drawBig(v = T.balance()) {
  $('#big').textContent = $('#navBal').textContent = T.fmt(v);
}
function drawForm() {
  const a = amount();
  $('#sMore').setAttribute('aria-checked', dir > 0);
  $('#sLess').setAttribute('aria-checked', dir < 0);
  $('#clear').hidden = !a;
  $('#dayText').textContent = sel === T.today ? 'Today' : T.label(sel);
  save.disabled = !a;
  save.textContent = a ? 'Save ' + T.fmt(dir * a) : 'Save';
}
function drawLists() {
  const list = [...T.entries].reverse();
  $('#recent').innerHTML = list.slice(0, 6).map(row).join('');
  // This week, month and quarter so far, not rolling windows.
  $('#trio').innerHTML = [['Week', 'week'], ['Month', 'month'], ['Quarter', 'quarter']].map(([k, unit]) => {
    const v = T.net(T.startOf(unit));
    return `<div><dt>${k}</dt><dd class="tn ${tone(v)}">${T.fmt(v)}</dd></div>`;
  }).join('');
}
function refresh(before) {
  drawLists();
  chart();
  T.tween(before, T.balance(), drawBig);
}
function commit(date, min, text, noteText) {
  const before = T.balance(), old = T.get(date);
  last = { date, min: old ? old.min : 0, note: old ? old.note : '' };
  T.set(date, min, noteText);
  navigator.vibrate?.(8);
  refresh(before);
  $('#toastText').textContent = text;
  $('#toast').classList.add('on');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => $('#toast').classList.remove('on'), 4000);
}

[h, m].forEach((el) => el.addEventListener('input', () => { el.value = el.value.replace(/\D/g, ''); drawForm(); }));
$('#sMore').addEventListener('click', () => { dir = 1; drawForm(); });
$('#sLess').addEventListener('click', () => { dir = -1; drawForm(); });
$$('[data-q]').forEach((b) => b.addEventListener('click', () => { setAmount(amount() + +b.dataset.q); drawForm(); }));
$('#clear').addEventListener('click', () => { setAmount(0); drawForm(); });
day.value = day.max = T.today;
day.min = T.start;
day.addEventListener('click', () => { try { day.showPicker(); } catch (_) { /* the native tap opens it */ } });
day.addEventListener('change', () => { sel = day.value && day.value <= T.today ? day.value : T.today; drawForm(); });
form.addEventListener('submit', (e) => {
  e.preventDefault();
  const a = amount(), old = T.get(sel);
  if (!a) return;
  commit(sel, (old ? old.min : 0) + dir * a, 'Saved ' + T.fmt(dir * a), note.value.trim() || (old ? old.note : ''));
  setAmount(0);
  note.value = '';
  drawForm();
});
app.addEventListener('click', (e) => {
  const b = e.target.closest('[data-del]');
  if (b) commit(b.dataset.del, 0, 'Deleted', '');
});
$('#undo').addEventListener('click', () => {
  if (!last) return;
  const before = T.balance();
  T.set(last.date, last.min, last.note);
  last = null;
  $('#toast').classList.remove('on');
  refresh(before);
});

// Stats. One period scopes both charts, the net figure and the list of days.
// It is this week, month, quarter or year so far, or any two dates.
let period = 'year', custom = { from: T.iso(T.addDays(T.parse(T.today), -59)), to: T.today };

function span() {
  if (period === 'custom') return custom;
  // A period never starts before the first entry.
  const from = T.startOf(period);
  return { from: from < T.start ? T.start : from, to: T.today };
}
function chart() {
  const sp = span(), inside = T.entries.filter((e) => e.date >= sp.from && e.date <= sp.to), net = inside.reduce((t, e) => t + e.min, 0);
  Charts.waterfall($('#plot'), sp);
  Charts.calendar($('#cal'), sp);
  $('#spanNet').className = 'tn ' + tone(net);
  $('#spanNet').textContent = T.fmt(net);
  $('#spanDays').textContent = `${inside.length} ${inside.length === 1 ? 'day' : 'days'} logged`;
  let month = '';
  $('#all').innerHTML = inside.reverse().map((e) => {
    const k = e.date.slice(0, 7), head = k === month ? '' : `<li class="mh">${T.parse(e.date).toLocaleString('en', { month: 'long' })}</li>`;
    month = k;
    return head + row(e);
  }).join('') || '<li class="mh">Nothing logged in this period</li>';
}
new ResizeObserver(() => Charts.waterfall($('#plot'), span())).observe($('#plot'));
// The calendar sets its own height, so only a change in width should redraw it.
let calWidth = 0;
new ResizeObserver(() => { const w = $('#cal').clientWidth; if (w !== calWidth) { calWidth = w; Charts.calendar($('#cal'), span()); } }).observe($('#cal'));

function drawPeriod() {
  $$('#range button').forEach((b) => b.setAttribute('aria-pressed', b.dataset.r === period));
  $('#custom').hidden = period !== 'custom';
  $('#from').value = custom.from;
  $('#to').value = custom.to;
  chart();
}
$('#range').addEventListener('click', (e) => {
  const b = e.target.closest('[data-r]');
  if (!b) return;
  period = b.dataset.r;
  drawPeriod();
});
['from', 'to'].forEach((id) => {
  const el = $('#' + id);
  el.min = T.start;
  el.max = T.today;
  el.addEventListener('change', () => {
    // Keep both ends inside the data and in order, whichever one was edited.
    let from = $('#from').value || T.start, to = $('#to').value || T.today;
    if (from < T.start) from = T.start;
    if (to > T.today) to = T.today;
    if (from > to) { if (id === 'from') to = from; else from = to; }
    custom = { from, to };
    drawPeriod();
  });
});

// Agents
$$('[data-copy]').forEach((b) => b.addEventListener('click', () => {
  navigator.clipboard?.writeText($('#' + b.dataset.copy).textContent).catch(() => {});
  b.textContent = 'Copied';
  setTimeout(() => { b.textContent = 'Copy'; }, 1200);
}));
$('#write').addEventListener('change', (e) => {
  $('#addTool').hidden = !e.target.checked;
  $('#access').textContent = e.target.checked ? 'Read and add, over MCP' : 'Read only, over MCP';
});

drawBig();
drawForm();
drawLists();
drawPeriod();
T.route((id) => {
  app.dataset.screen = id;
  $$('.links a').forEach((a) => (a.getAttribute('href') === '#' + id ? a.setAttribute('aria-current', 'page') : a.removeAttribute('aria-current')));
  $('.main').scrollTop = 0;
});
