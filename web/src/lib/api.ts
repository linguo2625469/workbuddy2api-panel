// 面板接口请求：统一带上访问密钥（Bearer，与 /v1/* 同一口径），401 时通知外壳弹出密钥框。

const KEY_STORAGE = 'wb2api.key'

export class ApiError extends Error {
  readonly status: number
  constructor(message: string, status: number) {
    super(message)
    this.status = status
  }
}

export function getKey(): string {
  try {
    return localStorage.getItem(KEY_STORAGE) ?? ''
  } catch {
    return ''
  }
}

export function setKey(key: string) {
  try {
    localStorage.setItem(KEY_STORAGE, key)
  } catch {
    // 无痕模式等拿不到存储：本页会话内每次都要重新输入
  }
}

/** 需要密钥时触发（外壳监听它弹出密钥框） */
export const AUTH_EVENT = 'wb2api:auth-required'

function authHeaders(): Record<string, string> {
  const k = getKey()
  return k ? { Authorization: 'Bearer ' + k } : {}
}

async function handle<T>(r: Response): Promise<T> {
  if (r.status === 401) {
    window.dispatchEvent(new Event(AUTH_EVENT))
    throw new ApiError('密钥无效或未填写', 401)
  }
  const d = (await r.json().catch(() => ({}))) as { error?: string }
  if (!r.ok) throw new ApiError(d.error || 'HTTP ' + r.status, r.status)
  return d as T
}

export async function api<T>(path: string, init: { method?: 'GET' | 'POST'; body?: unknown } = {}): Promise<T> {
  const headers = authHeaders()
  if (init.body !== undefined) headers['Content-Type'] = 'application/json'
  const r = await fetch('/panel/api/' + path, {
    method: init.method ?? 'GET',
    headers,
    body: init.body === undefined ? undefined : JSON.stringify(init.body),
  })
  return handle<T>(r)
}

export const post = <T>(path: string, body?: unknown) => api<T>(path, { method: 'POST', body })

export async function upload<T>(path: string, form: FormData): Promise<T> {
  const r = await fetch('/panel/api/' + path, { method: 'POST', body: form, headers: authHeaders() })
  return handle<T>(r)
}

export const errorMessage = (e: unknown) => (e instanceof Error ? e.message : String(e))

/** 账号接口路径 */
export const acct = (uid: string, action: string) => 'accounts/' + encodeURIComponent(uid) + '/' + action
