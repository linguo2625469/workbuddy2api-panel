// 展示格式化：缩写只做"一览"，精确值永远放 title（用量页可读性的核心约定）。
export function fmtTok(n) {
  n = Number(n || 0);
  if (n >= 1e9) return (n / 1e9).toFixed(2) + 'B';
  if (n >= 1e6) return (n / 1e6).toFixed(2) + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'k';
  return String(Math.round(n));
}

export function fmtInt(n) {
  const v = Number(n || 0);
  if (!Number.isFinite(v)) return '—';
  return Math.round(v).toLocaleString('en-US');
}

export function fmtPct(part, whole, digits = 1) {
  part = Number(part || 0);
  whole = Number(whole || 0);
  if (!whole) return '—';
  return ((part / whole) * 100).toFixed(digits) + '%';
}

export function fmtMs(ms) {
  ms = Number(ms || 0);
  if (!ms) return '—';
  if (ms >= 1000) return (ms / 1000).toFixed(2) + 's';
  return Math.round(ms) + 'ms';
}

export function fmtRate(r) {
  return r ? Number(r).toFixed(1) + ' tok/s' : '—';
}

export function ago(iso) {
  if (!iso || String(iso).startsWith('0001-')) return '—';
  const s = (Date.now() - new Date(iso).getTime()) / 1000;
  if (!Number.isFinite(s)) return '—';
  if (s < 0) return '刚刚';
  if (s < 60) return Math.floor(s) + ' 秒前';
  if (s < 3600) return Math.floor(s / 60) + ' 分钟前';
  if (s < 86400) return Math.floor(s / 3600) + ' 小时前';
  return Math.floor(s / 86400) + ' 天前';
}

export function dur(sec) {
  sec = Math.max(0, Math.round(Number(sec) || 0));
  const h = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const s = sec % 60;
  if (h) return h + '时' + String(m).padStart(2, '0') + '分';
  if (m) return m + '分' + String(s).padStart(2, '0') + '秒';
  return s + '秒';
}

export function shortUid(uid, n = 8) {
  uid = String(uid || '');
  return uid.length > n ? uid.slice(0, n) : uid;
}

export function fmtDayLabel(t) {
  const d = new Date(t);
  return d.getMonth() + 1 + '-' + String(d.getDate()).padStart(2, '0');
}

export function fmtHourLabel(t) {
  const d = new Date(t);
  return String(d.getHours()).padStart(2, '0') + ':00';
}
