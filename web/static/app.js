// tally app. Ported from design/reference/app.js and wired to the server.
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
// Notes and agent names come from people and agents, so they are escaped before they meet innerHTML.
const esc = (s) => String(s).replace(/[&<>"']/g, (c) => `&#${c.charCodeAt(0)};`);
$$('[data-i]').forEach((el) => { el.innerHTML = icon(el.dataset.i, +el.dataset.s || 22); });

// Add. Any amount, saved onto the picked day. Undo instead of confirm.
let dir = 1, sel = T.today, last = null, toastTimer, shown = T.balance(), changes = 0;
const app = $('#app'), form = $('#form'), h = $('#h'), m = $('#m'), note = $('#note'), day = $('#day'), save = $('#save');

const amount = () => (parseInt(h.value, 10) || 0) * 60 + (parseInt(m.value, 10) || 0);
function setAmount(min) {
  h.value = min >= 60 ? Math.floor(min / 60) : '';
  m.value = min % 60 || '';
}
const dayName = (date) => (date === T.today ? 'Today' : T.label(date));
// The day and its note are a button: it puts that day in the form, to add to it.
const row = (e) => `<li><button type="button" class="grow open" data-pick="${esc(e.date)}"${e.date === sel && sel !== T.today ? ' aria-current="date"' : ''}><b>${dayName(e.date)}</b>${e.note ? `<small>${esc(e.note)}</small>` : ''}</button><span class="v tn ${tone(e.min)}">${T.fmt(e.min)}</span><button class="del" data-del="${esc(e.date)}" aria-label="Delete ${T.label(e.date)}">${icon('x', 18)}</button></li>`;
const empty = (text) => `<li class="mh">${text}</li>`;

function drawBig(v = T.balance()) {
  shown = v;
  $('#big').textContent = $('#navBal').textContent = T.fmt(v);
}
function drawForm() {
  const a = amount();
  $('#sMore').setAttribute('aria-checked', dir > 0);
  $('#sLess').setAttribute('aria-checked', dir < 0);
  $('#clear').hidden = !a;
  $('#dayText').textContent = dayName(sel);
  save.disabled = !a;
  save.textContent = a ? 'Save ' + T.fmt(dir * a) : 'Save';
  // Saving adds to a day, so the form says what the day holds and what it will hold.
  const e = T.get(sel), has = $('#has');
  has.hidden = !e;
  if (e) {
    const after = e.min + dir * a;
    has.innerHTML = `Has <b class="${tone(e.min)}">${T.fmt(e.min)}</b>` + (a ? `, becomes <b class="${tone(after)}">${T.fmt(after)}</b>` : '');
  }
}

// A past day put in the form is marked where it shows: its calendar cell, and its row in a
// list. Today needs no mark, the form already says Today.
function markSel() {
  const past = sel !== T.today;
  $$('#cal rect[data-d]').forEach((r) => r.classList.toggle('sel', past && r.dataset.d === sel));
  $$('[data-pick]').forEach((b) => (past && b.dataset.pick === sel ? b.setAttribute('aria-current', 'date') : b.removeAttribute('aria-current')));
}
const wide = () => app.clientWidth >= 860;
// Put a day in the form. On a phone the form is on Add, so a day picked on Stats goes there.
// On a desktop the form is beside the charts, and the cursor goes to the hours.
function pick(date) {
  if (date > T.today) return;
  sel = day.value = date;
  drawForm();
  markSel();
  if (wide()) h.focus({ preventScroll: true });
  else if (app.dataset.screen !== 'today') location.hash = '#today';
}
Charts.onPick = (date) => { if (wide()) pick(date); };
function drawLists() {
  const list = [...T.entries].reverse();
  $('#recent').innerHTML = list.slice(0, 6).map(row).join('') || empty('Nothing logged yet');
  // This week, month and quarter so far, not rolling windows.
  $('#trio').innerHTML = [['Week', 'week'], ['Month', 'month'], ['Quarter', 'quarter']].map(([k, unit]) => {
    const v = T.net(T.startOf(unit));
    return `<div><dt>${k}</dt><dd class="tn ${tone(v)}">${T.fmt(v)}</dd></div>`;
  }).join('');
}
function refresh() {
  drawLists();
  chart();
  T.tween(shown, T.balance(), drawBig);
}
// The toast is a live region that stays in place, so screen readers announce what it says.
// Only Undo comes and goes with it. toastFor is the change the toast is about.
let toastFor = 0;
function toast(text, undo, id = 0) {
  toastFor = id;
  $('#toastText').textContent = text;
  $('#undo').hidden = !undo;
  $('#undo').inert = false;
  $('#toast').classList.add('on');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(hideToast, 4000);
}
// Once the toast has gone, so has Undo, for the keyboard and screen readers too.
function hideToast() {
  clearTimeout(toastTimer);
  if ($('#toast').contains(document.activeElement)) $('.main').focus({ preventScroll: true });
  $('#toast').classList.remove('on');
  $('#undo').inert = true;
  last = null;
}
$('#undo').inert = true;
// The entry a day holds, the way the server writes it.
const entry = (min, note) => (min ? { minutes: min, note } : null);
// Change a day here at once, send the change behind it, and offer Undo. The toast comes first,
// so nothing drawn after it can hide that the change was made.
function commit(date, min, text, noteText, method, path, body, fail) {
  const old = T.get(date), id = ++changes, target = entry(min, noteText);
  T.set(date, min, noteText);
  toast(text, true, id);
  const done = T.send(method, path, body, { id, fail, date, target });
  last = { id, date, min: old ? old.min : 0, note: old ? old.note : '', done, target };
  navigator.vibrate?.(8);
  refresh();
}

[h, m].forEach((el) => el.addEventListener('input', () => { el.value = el.value.replace(/\D/g, ''); drawForm(); }));
// 90 minutes reads as 1h 30m once the field is left.
m.addEventListener('blur', () => { if (amount() && (+m.value >= 60)) { setAmount(amount()); drawForm(); } });
$('#sMore').addEventListener('click', () => { dir = 1; drawForm(); });
$('#sLess').addEventListener('click', () => { dir = -1; drawForm(); });
$$('[data-q]').forEach((b) => b.addEventListener('click', () => { setAmount(amount() + +b.dataset.q); drawForm(); }));
$('#clear').addEventListener('click', () => { setAmount(0); drawForm(); });
day.value = day.max = T.today;
day.min = '2000-01-01';
day.addEventListener('click', () => { try { day.showPicker(); } catch (_) { /* the native tap opens it */ } });
day.addEventListener('change', () => {
  // Typing a year passes through ones like 0002, which are not taken.
  if (day.value && day.value < day.min) return;
  sel = day.value && day.value <= T.today ? day.value : T.today;
  drawForm();
  markSel();
});
day.addEventListener('blur', () => { day.value = sel; });
form.addEventListener('submit', (e) => {
  e.preventDefault();
  const a = amount(), old = T.get(sel), date = sel, text = note.value.trim();
  if (!a) return;
  // The toast names the day when it is not today, which matters when filling in past days.
  commit(date, (old ? old.min : 0) + dir * a, 'Saved ' + T.fmt(dir * a) + (date === T.today ? '' : ', ' + T.label(date)), text || (old ? old.note : ''),
    'POST', `/api/days/${date}/add`, { minutes: dir * a, note: text }, 'Not saved');
  setAmount(0);
  note.value = '';
  drawForm();
});
app.addEventListener('click', (e) => {
  const p = e.target.closest('[data-pick]');
  if (p) { pick(p.dataset.pick); return; }
  const b = e.target.closest('[data-del]');
  if (!b) return;
  const date = b.dataset.del;
  commit(date, 0, 'Deleted', '', 'DELETE', `/api/days/${date}`, undefined, 'Not deleted');
});
$('#undo').addEventListener('click', () => {
  if (!last) return;
  const { date, min, note: text, done, target } = last, id = ++changes;
  T.set(date, min, text);
  // Put back what the server had before the change, and only while the day still holds what the
  // change left. A change made in between, from another device or an agent, is never lost.
  // Without the server's answer to the change, what this app expected of it stands in.
  T.send('PUT', `/api/days/${date}`, async () => {
    const ch = await done.catch(() => null);
    if (!ch) return { minutes: min, note: text, expect: target };
    return { minutes: ch.before ? ch.before.minutes : 0, note: ch.before ? ch.before.note : '', expect: ch.after };
  }, { id, fail: 'Not undone', date, target: entry(min, text) });
  hideToast();
  toastFor = id;
  refresh();
});

// Stats. One period scopes both charts, the net figure and the list of days.
// It is this week, month, quarter or year so far, or any two dates.
let period = 'year', custom = { from: T.iso(T.addDays(T.parse(T.today), -59)), to: T.today };

// Both ends stay inside the data and in order. A period never starts before the first entry.
const inside = (d) => (d < T.start ? T.start : d > T.today ? T.today : d);
function span() {
  const from = inside(period === 'custom' ? custom.from : T.startOf(period)), to = inside(period === 'custom' ? custom.to : T.today);
  return { from, to: to < from ? from : to };
}
// The calendar of a week, month or quarter runs to its last day, with the days to come as
// outlines, so early in a period it is not a single lonely cell.
function until() {
  const t = T.parse(T.today), y = t.getFullYear(), m = t.getMonth();
  if (period === 'week') return T.iso(T.addDays(T.parse(T.startOf('week')), 6));
  if (period === 'month') return T.iso(new Date(y, m + 1, 0));
  if (period === 'quarter') return T.iso(new Date(y, m - (m % 3) + 3, 0));
  return '';
}
function chart() {
  const sp = span(), inside = T.entries.filter((e) => e.date >= sp.from && e.date <= sp.to), net = inside.reduce((t, e) => t + e.min, 0);
  Charts.waterfall($('#plot'), sp);
  Charts.calendar($('#cal'), sp, until());
  markSel();
  $('#spanNet').className = 'tn ' + tone(net);
  $('#spanNet').textContent = T.fmt(net);
  $('#spanDays').textContent = `${inside.length} ${inside.length === 1 ? 'day' : 'days'} logged`;
  const year = T.today.slice(0, 4);
  let month = '';
  $('#all').innerHTML = inside.reverse().map((e) => {
    const k = e.date.slice(0, 7), name = T.parse(e.date).toLocaleString('en', { month: 'long' });
    const head = k === month ? '' : `<li class="mh">${k.slice(0, 4) === year ? name : `${name} ${k.slice(0, 4)}`}</li>`;
    month = k;
    return head + row(e);
  }).join('') || empty('Nothing logged in this period');
}
new ResizeObserver(() => Charts.waterfall($('#plot'), span())).observe($('#plot'));
// The calendar sets its own height, so only a change in width should redraw it.
let calWidth = 0;
new ResizeObserver(() => { const w = $('#cal').clientWidth; if (w !== calWidth) { calWidth = w; Charts.calendar($('#cal'), span(), until()); markSel(); } }).observe($('#cal'));

// A date field being typed into is left alone, limits included: Chrome ends the edit of a date
// field whose min, max or value is set meanwhile. It is tidied when it loses focus. The field
// is tracked by focus and blur, because while Chrome moves between the parts of a date,
// document.activeElement reads as the body.
let typing = null;
function fitDate(el) {
  el.min = T.start;
  el.max = T.today;
  el.value = custom[el.id];
}
function drawPeriod() {
  $$('#range button').forEach((b) => b.setAttribute('aria-pressed', b.dataset.r === period));
  $('#custom').hidden = period !== 'custom';
  if (period === 'custom') custom = span();
  ['#from', '#to'].map((id) => $(id)).filter((el) => el !== typing).forEach(fitDate);
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
  el.addEventListener('change', () => {
    // Typing a date fires a change for each part of it, years like 0002 included. Only whole,
    // plausible dates count.
    if (!/^\d{4}-\d{2}-\d{2}$/.test(el.value) || el.value < '2000-01-01') return;
    // Keep both ends inside the data and in order, whichever one was edited.
    let from = inside(id === 'from' ? el.value : custom.from), to = inside(id === 'to' ? el.value : custom.to);
    if (from > to) { if (id === 'from') to = from; else from = to; }
    custom = { from, to };
    drawPeriod();
  });
  el.addEventListener('focus', () => { typing = el; });
  el.addEventListener('blur', () => { typing = null; fitDate(el); });
});

// Agents
const TOOL = { get_balance: 'Balance', list_days: 'Days', get_trend: 'Trend', add_day: 'Add' };
// How long ago, by the server's clock and in its zone, the same as today.
function ago(at) {
  const t = new Date(at).getTime(), s = (T.now() - t) / 1000, day = T.dayOf(t);
  if (s < 60) return 'now';
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (day === T.today) return `${Math.floor(s / 3600)}h ago`;
  if (day === T.iso(T.addDays(T.parse(T.today), -1))) return 'yesterday';
  return T.label(day);
}
function drawAgents() {
  const a = T.agents;
  $('#url').textContent = a.url;
  $('#tok').textContent = a.token ? 'tly_••••••••' + a.token.slice(-4) : '';
  $('#write').checked = a.write;
  $('#addTool').hidden = !a.write;
  $('#access').textContent = a.write ? 'Read and add, over MCP' : 'Read only, over MCP';
  $('#conn').innerHTML = a.connected.map((c) => `<li><span class="grow"><b>${esc(c.name)}</b></span><span class="when">${[TOOL[c.tool], ago(c.at)].filter(Boolean).join(', ')}</span></li>`).join('') || empty('None yet');
}
function copy(text) {
  if (navigator.clipboard && window.isSecureContext) return navigator.clipboard.writeText(text).catch(() => copyOld(text));
  copyOld(text);
  return Promise.resolve();
}
// Plain http has no clipboard API, so select a hidden field and copy that.
function copyOld(text) {
  const t = document.createElement('textarea');
  t.value = text;
  t.setAttribute('readonly', '');
  t.style.position = 'fixed';
  t.style.opacity = '0';
  document.body.appendChild(t);
  t.select();
  try { document.execCommand('copy'); } catch (_) { /* nothing else to try */ }
  t.remove();
}
$$('[data-copy]').forEach((b) => b.addEventListener('click', () => {
  copy(b.dataset.copy === 'tok' ? T.agents.token : T.agents.url);
  b.textContent = 'Copied';
  setTimeout(() => { b.textContent = 'Copy'; }, 1200);
}));
$('#write').addEventListener('change', (e) => {
  const on = e.target.checked;
  T.agents.write = on;
  drawAgents();
  T.send('PUT', '/api/agents/write', { on }, { id: ++changes, fail: 'Not changed', held: () => T.agents.write === on });
});

// Keeping up. After a burst of changes the server's state is pulled and adopted, which also
// brings in anything agents added. The same happens when the app comes back into view, and
// every minute while it is in view. Only the parts that changed are drawn again, so an open
// chart readout stays open while an agent is busy.
let knownToday = T.today;
function adopt(changed) {
  if (T.today !== knownToday) {
    // The day picked as today stays today.
    if (sel === knownToday) sel = T.today;
    knownToday = day.max = T.today;
    day.value = sel;
    changed.entries = true;
  }
  if (changed.entries) {
    drawForm();
    drawLists();
    drawPeriod();
    T.tween(shown, T.balance(), drawBig);
  }
  if (changed.agents) drawAgents();
}
// A change that did not go through says so. When it is not the change the toast is showing,
// the toast names its day, so a later save that worked is not mistaken for the one that failed.
T.onSettled = (failed, changed) => {
  const f = failed[failed.length - 1];
  if (f) {
    const named = failed.length > 1 || f.id !== toastFor;
    hideToast();
    toast(named && f.date ? `${f.fail}, ${dayName(f.date)}` : f.fail, false);
  }
  adopt(changed);
};
async function sync() {
  if (T.tick()) adopt({});
  if (T.busy() || document.visibilityState !== 'visible') return;
  const changed = await T.pull();
  if (changed) adopt(changed);
}
document.addEventListener('visibilitychange', sync);
addEventListener('pageshow', (e) => { if (e.persisted) sync(); });
addEventListener('online', sync);
setInterval(() => {
  sync();
  // "2m ago" should not stay "now".
  if (app.dataset.screen === 'agents') drawAgents();
}, 60000);

drawBig();
drawForm();
drawLists();
drawPeriod();
drawAgents();
// On a desktop the cursor waits in the hours, so a day can be logged from the keyboard:
// 1, Tab, 30, Enter. A phone would pop up its keyboard for it, so it does not.
if (wide() && (location.hash || '#today') !== '#agents') h.focus({ preventScroll: true });
// On a desktop Add and Stats are one screen, Time, whose tab stays marked for both. The width
// that decides it is the app's, the same as for the layout.
function markNav() {
  const id = app.dataset.screen, current = id === 'balance' && app.clientWidth >= 860 ? 'today' : id;
  $$('.links a').forEach((a) => (a.getAttribute('href') === '#' + current ? a.setAttribute('aria-current', 'page') : a.removeAttribute('aria-current')));
}
new ResizeObserver(markNav).observe(app);
T.route((id) => {
  app.dataset.screen = id;
  markNav();
  $('.main').scrollTop = 0;
  if (id === 'agents') drawAgents();
});
