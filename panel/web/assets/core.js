'use strict';
// Shared helpers for every view: DOM, API calls, the polled data store,
// formatting, signal-quality scales, toasts and routing.

const $ = (sel, root) => (root || document).querySelector(sel);
const $$ = (sel, root) => Array.from((root || document).querySelectorAll(sel));

function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}
function icon(name, cls) {
  return `<svg class="i${cls ? ' ' + cls : ''}"><use href="#i-${name}"/></svg>`;
}
function setText(id, v) {
  const el = typeof id === 'string' ? document.getElementById(id) : id;
  if (el) el.textContent = v == null || v === '' ? '—' : v;
}

// ------------------------------------------------------------------ API ---

class ApiError extends Error {}

async function api(path, body) {
  const opts = { headers: { 'X-CPE-Box': '1' } };
  if (body !== undefined) {
    opts.method = 'POST';
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  let res, json;
  try {
    res = await fetch(path, opts);
  } catch (e) {
    throw new ApiError('The panel is not responding - is CPE Box still running?');
  }
  try { json = await res.json(); } catch (e) { throw new ApiError(`Unexpected response (${res.status})`); }
  if (res.status === 401 && json.login) {
    location.href = '/login';
    throw new ApiError('Sign-in required');
  }
  if (!json.ok) throw new ApiError(cleanError(json.error || 'Request failed'));
  return json.data;
}

// A stock (Xiaomi web UI) action; refreshes the given sources afterwards.
async function stock(action, fields, refreshes) {
  const res = await api('/api/stock?a=' + action, fields || {});
  (refreshes || []).forEach(n => refresh(n));
  return res;
}

// cjson turns an empty Lua list into {} - always hand back an array.
const arr = v => Array.isArray(v) ? v : [];

// SSH errors come back with ssh's own chatter in front; keep the useful part.
function cleanError(msg) {
  return String(msg).replace(/Warning: Permanently added[^\n]*\n?/g, '').replace(/\r/g, '').trim();
}

// ---------------------------------------------------------- data store ---
// Every read endpoint is a "source". Views subscribe to the sources they
// show; the poller only refreshes sources the current view needs (plus the
// header's), so an open tab costs the router as little as possible. The
// server caches these reads too, shared by every open browser.

const Sources = {
  status:   { url: '/api/status',         every: 20000 },
  cellular: { url: '/api/cellular-info',  every: 15000 },
  connectivity: { url: '/api/connectivity', every: 30000 },
  health:   { url: '/api/system-health',  every: 15000 },
  usage:    { url: '/api/data-usage',     every: 5000 },
  devices:  { url: '/api/device-monitor', every: 30000 },
  leds:     { url: '/api/leds',           every: 60000 },
  info:     { url: '/api/info',           every: 60000 },
  ssh:      { url: '/api/ssh-info',       every: Infinity },
  // the stock Xiaomi settings, read through the router's own web code
  wifiCfg:  { url: '/api/stock?a=wifi',    every: 60000 },
  hosts:    { url: '/api/stock?a=hosts',   every: 30000 },
  apn:      { url: '/api/stock?a=apn',     every: 120000 },
  netcfg:   { url: '/api/stock?a=netcfg',  every: 120000 },
  pin:      { url: '/api/stock?a=pin',     every: 120000 },
  autopin:  { url: '/api/stock?a=autopin', every: 120000 },
  simstatus: { url: '/api/stock?a=simstatus', every: 15000 },
  sms:      { url: '/api/stock?a=sms',     every: 30000 },
  contacts: { url: '/api/contacts',        every: 300000 },
  lan:      { url: '/api/stock?a=lan',     every: 300000 },
  dhcp:     { url: '/api/stock?a=dhcp',    every: 300000 },
  upnp:     { url: '/api/stock?a=upnp',    every: 60000 },
  dmz:      { url: '/api/stock?a=dmz',     every: 300000 },
  portfwd:  { url: '/api/stock?a=portfwd', every: 300000 },
  rname:    { url: '/api/stock?a=name',    every: 300000 },
};
for (const s of Object.values(Sources)) Object.assign(s, { data: null, error: null, at: 0, inflight: null, subs: [] });

function on(name, fn) {
  Sources[name].subs.push(fn);
  const s = Sources[name];
  if (s.data || s.error) fn(s.data, s.error);
}

function publish(name, data) {
  const s = Sources[name];
  s.data = data; s.error = null; s.at = Date.now();
  s.subs.forEach(fn => { try { fn(data, null); } catch (e) { console.error(e); } });
}

function refresh(name) {
  const s = Sources[name];
  if (s.inflight) return s.inflight;
  s.inflight = api(s.url).then(
    data => { publish(name, data); },
    err => {
      s.error = err; s.at = Date.now();
      s.subs.forEach(fn => { try { fn(s.data, err); } catch (e) { console.error(e); } });
    },
  ).finally(() => { s.inflight = null; Hooks.afterRefresh(); });
  return s.inflight;
}

const Hooks = { afterRefresh() {} };

// ---------------------------------------------------------- formatting ---

function fmtBytes(b) {
  if (b == null || b < 0 || isNaN(b)) return '—';
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  while (b >= 1000 && i < u.length - 1) { b /= 1000; i++; }
  return (i === 0 ? b : b >= 100 ? b.toFixed(0) : b.toFixed(1)) + ' ' + u[i];
}
function fmtBytesParts(b) {
  const s = fmtBytes(b);
  const i = s.lastIndexOf(' ');
  return i < 0 ? [s, ''] : [s.slice(0, i), s.slice(i + 1)];
}
function fmtUptime(sec) {
  if (sec == null) return '—';
  const d = Math.floor(sec / 86400), h = Math.floor((sec % 86400) / 3600), m = Math.floor((sec % 3600) / 60);
  if (d) return `${d}d ${h}h`;
  if (h) return `${h}h ${m}m`;
  return `${m}m`;
}
function num(v) {
  const n = parseFloat(v);
  return isNaN(n) ? null : n;
}

// ------------------------------------------------------ signal quality ---
// Commonly used LTE/NR bands for RSRP/RSRQ/SINR. Returns a class for
// colour, a 0-100 fill and a word.

const Q = {
  rsrp: { min: -125, max: -70, steps: [-80, -90, -100] },
  rsrq: { min: -22, max: -5, steps: [-10, -15, -20] },
  snr:  { min: -5, max: 30, steps: [20, 13, 0] },
  rssi: { min: -110, max: -50, steps: [-65, -75, -85] },
};
const Q_LABELS = [['q-good', 'Excellent'], ['q-ok', 'Good'], ['q-fair', 'Fair'], ['q-poor', 'Poor']];

function quality(metric, v) {
  v = num(v);
  if (v == null || v === 0) return null;
  const q = Q[metric];
  let i = q.steps.findIndex(s => v >= s);
  if (i < 0) i = 3;
  const pct = Math.max(4, Math.min(100, (v - q.min) / (q.max - q.min) * 100));
  // 5 bars: excellent 5 ... poor 2, very poor 1
  const level = i === 3 && pct < 20 ? 1 : 5 - i;
  return { cls: Q_LABELS[i][0], label: Q_LABELS[i][1], pct, level };
}

function meterRow(name, value, unit, metric) {
  const q = metric ? quality(metric, value) : null;
  const v = num(value);
  // SINR of exactly 0 dB is a real (poor) reading, not "no data" the way a 0 for
  // RSRP/RSRQ is (those are never 0 for a live signal), so don't blank it.
  const shown = v == null || (metric && metric !== 'snr' && v === 0) ? '—' : `${v}<small> ${unit}</small>`;
  return `<div class="meter"><div class="top"><span>${esc(name)}</span><b>${shown}</b></div>
    <div class="track"><div class="fill ${q ? q.cls : ''}" style="width:${q ? q.pct : 0}%"></div></div></div>`;
}

// ring gauge for a plain percentage (CPU, memory, temperature)
function gauge(name, pct, valueHtml, detail, warnAt, badAt) {
  const cls = pct == null ? '' : pct >= badAt ? 'q-poor' : pct >= warnAt ? 'q-fair' : '';
  const p = pct == null ? 0 : Math.max(1, Math.min(100, pct));
  return `<div><div class="gauge-wrap"><div class="gauge ${cls}" style="--p:${p.toFixed(1)}"></div>
    <div class="gauge-val">${valueHtml == null ? '—' : valueHtml}</div></div>
    <div class="gauge-l">${esc(name)}</div><div class="gauge-d">${detail ? esc(detail) : '&nbsp;'}</div></div>`;
}

function setBars(el, level, cls) {
  el.className = el.className.replace(/\bq-\w+/g, '').trim() + (cls ? ' ' + cls : '');
  Array.from(el.children).forEach((b, i) => b.classList.toggle('on', i < level));
}
function barsHtml(level, cls) {
  let s = `<span class="bars ${cls || ''}">`;
  for (let i = 1; i <= 5; i++) s += `<i class="${i <= level ? 'on' : ''}"></i>`;
  return s + '</span>';
}

// ------------------------------------------------------------ feedback ---

function toast(text, kind) {
  const el = document.createElement('div');
  el.className = 'toast ' + (kind || 'ok');
  el.innerHTML = `<span class="dot"></span><span>${esc(text)}</span>`;
  $('#toasts').appendChild(el);
  setTimeout(() => el.remove(), kind === 'err' ? 7000 : 3500);
}

function setMsg(id, text, kind) {
  const el = document.getElementById(id);
  if (!el) return;
  el.textContent = text || '';
  el.className = 'msg' + (kind ? ' ' + kind : '');
}

// Runs fn with the button showing a spinner; errors become a toast.
async function withBusy(btn, fn) {
  if (btn) { btn.disabled = true; btn.classList.add('busy'); }
  try {
    return await fn();
  } catch (e) {
    toast(e.message, 'err');
    throw e;
  } finally {
    if (btn) { btn.disabled = false; btn.classList.remove('busy'); }
  }
}

async function copyText(text, btn) {
  try {
    await navigator.clipboard.writeText(text);
  } catch (e) {
    const ta = document.createElement('textarea');
    ta.value = text; document.body.appendChild(ta); ta.select();
    document.execCommand('copy'); ta.remove();
  }
  if (btn) {
    const old = btn.innerHTML;
    btn.innerHTML = icon('check');
    setTimeout(() => { btn.innerHTML = old; }, 1200);
  }
}
document.addEventListener('click', e => {
  const b = e.target.closest('[data-copy]');
  if (b) copyText(document.getElementById(b.dataset.copy).textContent, b);
});

// --------------------------------------------------------------- theme ---

function currentTheme() {
  return document.documentElement.dataset.theme === 'light' ? 'light' : 'dark';
}
function paintThemeButton() {
  $('#themeBtn').innerHTML = icon(currentTheme() === 'dark' ? 'sun' : 'moon');
}
function toggleTheme() {
  const next = currentTheme() === 'dark' ? 'light' : 'dark';
  if (next === 'light') document.documentElement.dataset.theme = 'light';
  else delete document.documentElement.dataset.theme;
  try { localStorage.setItem('cpebox-theme', next); } catch (e) {}
  paintThemeButton();
}

// ------------------------------------------------------------- routing ---
// Each view declares the sources it shows; see main.js.

const Views = {};
let currentView = null;

// The hash may carry a query (#messages?to=+49...), handed to the view.
let viewQuery = new URLSearchParams();
function showView(name) {
  const q = name.indexOf('?');
  viewQuery = new URLSearchParams(q >= 0 ? name.slice(q + 1) : '');
  if (q >= 0) name = name.slice(0, q);
  if (!Views[name]) name = 'overview';
  currentView = name;
  $$('.view').forEach(v => { v.hidden = v.id !== 'v-' + name; });
  $$('.tabs a, .tabbar a').forEach(a => {
    if (a.getAttribute('href') === '#' + name) a.setAttribute('aria-current', 'page');
    else a.removeAttribute('aria-current');
  });
  document.title = $('#v-' + name).dataset.title + ' · CPE Box';
  if (Views[name].show) Views[name].show();
  window.scrollTo(0, 0);
}
