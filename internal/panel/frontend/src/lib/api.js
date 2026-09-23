// 面板 API 客户端：与网关 api_key 同口径 Bearer 鉴权（401 即弹密钥门）。
const LS_KEY = 'wb2api.key';

export function getKey() {
  return localStorage.getItem(LS_KEY) || '';
}

export function setKey(v) {
  localStorage.setItem(LS_KEY, v);
}

export async function api(path, opts = {}) {
  const headers = { ...(opts.headers || {}) };
  const k = getKey();
  if (k) headers['Authorization'] = 'Bearer ' + k;
  let body;
  if (opts.body !== undefined) {
    headers['Content-Type'] = 'application/json';
    body = typeof opts.body === 'string' ? opts.body : JSON.stringify(opts.body);
  }
  const r = await fetch('/panel/api/' + path, {
    method: opts.method || 'GET',
    headers,
    body
  });
  if (r.status === 401) {
    const e = new Error('密钥无效或未填写');
    e.unauthorized = true;
    throw e;
  }
  const d = await r.json().catch(() => ({}));
  if (!r.ok) throw new Error(d.error || 'HTTP ' + r.status);
  return d;
}
