/* zcode2api dashboard — shared: token gate, header, fetch, toasts, helpers */
const LINKS = { github: 'https://github.com/D3-vin/Zcode2Api', telegram: 'https://t.me/D3_vin' };
var DASH_TOKEN = localStorage.getItem('dash_token') || '';
var authWaiters = [];

function dashHeaders(extra) {
  var h = extra || {};
  if (DASH_TOKEN) h['Authorization'] = 'Bearer ' + DASH_TOKEN;
  return h;
}
var DBG = localStorage.getItem('dash_debug') === '1';
function dbg(...a) { if (DBG) console.log('[dash]', ...a); }

async function j(url, opts) {
  opts = opts || {};
  opts.headers = dashHeaders(opts.headers || {});
  dbg('GET/POST', url);
  var res = await fetch(url, opts);
  dbg('←', url, res.status);
  if (res.status === 401) { dbg('401 → login'); localStorage.removeItem('dash_token'); location.href = 'login.html'; }
  var text = await res.text();
  dbg('body', url, text.slice(0, 300));
  var d;
  try { d = JSON.parse(text); }
  catch (e) {
    console.error('[dash] bad JSON from', url, '→', text.slice(0, 300), e);
    toast('Server returned non-JSON from ' + url, 'err');
    throw e;
  }
  return d;
}
function post(url, body) { return j(url, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body || {}) }); }
function toast(msg, tone) {
  const c = document.getElementById('toasts'); if (!c) return;
  const t = document.createElement('div'); t.className = 'toast ' + (tone || ''); t.textContent = msg;
  c.appendChild(t); setTimeout(() => t.remove(), 2600);
}
function showLogin(msg) { location.href = 'login.html'; }
function hideLogin() {
  const s = document.getElementById('loginScreen'); if (s) s.style.display = 'none';
  const a = document.getElementById('app'); if (a) a.style.display = '';
}
async function verifyToken(t) {
  const res = await fetch('api/status', { headers: { 'Authorization': 'Bearer ' + t } });
  return res.status === 200;
}

function doLogout() { DASH_TOKEN = ''; localStorage.removeItem('dash_token'); location.href = 'login.html'; }
function spinBtn(btn) {
  const s = btn && btn.querySelector('svg'); if (!s) return;
  s.classList.add('spin'); setTimeout(() => s.classList.remove('spin'), 1500);
}
function openModal(id) { document.getElementById(id).classList.add('open'); }
function closeModal(id) { document.getElementById(id).classList.remove('open'); }

function renderHeader(active) {
  // header is static HTML now; just mark the active nav link + version
  document.querySelectorAll('.admin-nav-link').forEach(a => a.classList.remove('active'));
  const map = { accounts: './', proxies: 'proxies.html', settings: 'settings.html' };
  const link = document.querySelector('.admin-nav-link[href="' + (map[active] || '/') + '"]');
  if (link) link.classList.add('active');
  j('api/status').then(s => { const v = document.getElementById('hdr-version'); if (v && s.version) v.textContent = 'v' + s.version; }).catch(() => {});
}
function fmtNum(v) {
  const n = Number(v); if (!isFinite(n)) return '—';
  if (Math.abs(n) >= 1e9) return (n / 1e9).toFixed(2) + 'B';
  if (Math.abs(n) >= 1e6) return (n / 1e6).toFixed(2) + 'M';
  if (Math.abs(n) >= 1e3) return (n / 1e3).toFixed(2) + 'K';
  return n.toLocaleString('en-US');
}
function fmtDateQ(d) { 
  if (!d) return ''; const dt = new Date(d * 1000); if (isNaN(dt)) return '';
  return dt.toLocaleString('en-US', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit' });
}
function fmtDate(ts) {
  if (!ts) return '';
  const d = new Date(ts * 1000); if (isNaN(d)) return String(ts);
  return d.getFullYear() + '-' + String(d.getMonth() + 1).padStart(2, '0') + '-' + String(d.getDate()).padStart(2, '0');
}
function tickClock() {
  const el = document.getElementById('updated-at'); if (!el) return;
  const d = new Date();
  el.textContent = 'Updated at ' + [d.getHours(), d.getMinutes(), d.getSeconds()].map(x => String(x).padStart(2, '0')).join(':');
}
function gate(onAuthed) {
  console.log('[dash] gate start, page=' + PAGE + ', token=' + (DASH_TOKEN ? 'yes' : 'no'));
  renderHeader(PAGE);
  (async () => {
    if (DASH_TOKEN && await verifyToken(DASH_TOKEN)) {
    const app = document.getElementById('app');
    if (app) app.style.display = '';
    boot(onAuthed);
    return;
  }
    DASH_TOKEN = ''; localStorage.removeItem('dash_token'); location.href = 'login.html';
  })();
}
