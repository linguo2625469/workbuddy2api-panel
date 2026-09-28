// 展示用的格式化函数（从旧版 app.js 平移，口径不变）。

/** Go 的 time.Time 零值（0001-01-01…）或空串都算「没有这个时间」 */
export function isZeroTime(iso?: string | null): boolean {
  return !iso || iso.startsWith('0001-')
}

/** 时间戳（毫秒）；零值、空串、解析失败返回 null */
export function timeMs(iso?: string | null): number | null {
  if (isZeroTime(iso)) return null
  const t = new Date(iso!).getTime()
  return Number.isFinite(t) ? t : null
}

export function ago(iso?: string | null, now = Date.now()): string {
  const t = timeMs(iso)
  if (t == null) return '—'
  const s = (now - t) / 1000
  if (s < 0) return '刚刚'
  if (s < 60) return Math.floor(s) + ' 秒前'
  if (s < 3600) return Math.floor(s / 60) + ' 分钟前'
  if (s < 86400) return Math.floor(s / 3600) + ' 小时前'
  return Math.floor(s / 86400) + ' 天前'
}

/** 秒数 → 「1时05分」「3分07秒」「12秒」 */
export function dur(sec: number): string {
  sec = Math.max(0, Math.round(sec))
  const h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60), s = sec % 60
  if (h) return h + '时' + String(m).padStart(2, '0') + '分'
  if (m) return m + '分' + String(s).padStart(2, '0') + '秒'
  return s + '秒'
}

export function uptime(sec: number): string {
  const up = Math.floor(sec)
  return '运行 ' + (up >= 86400 ? Math.floor(up / 86400) + ' 天 ' : '') +
    Math.floor((up % 86400) / 3600) + ' 时 ' + Math.floor((up % 3600) / 60) + ' 分'
}

/** 账号用量里的 token 数：999 → 999，1234 → 1.2k，999999 → 1m（四舍五入后自动升级单位） */
export function formatTokenCount(tokens?: number | null): string {
  if (tokens == null) return '—'
  const n = Number(tokens)
  if (!Number.isFinite(n) || n < 0) return '—'
  if (n < 1000) return String(Math.round(n))
  const units: [string, number][] = [['k', 1e3], ['m', 1e6], ['b', 1e9]]
  let i = 0
  for (let j = 0; j < units.length; j++) if (n >= units[j][1]) i = j
  let rounded = Number((n / units[i][1]).toFixed(1))
  if (i + 1 < units.length && rounded >= 1000) {
    i++
    rounded = Number((n / units[i][1]).toFixed(1))
  }
  return rounded + units[i][0]
}

export function formatLatency(ms?: number | null): string {
  if (ms == null) return '—'
  const n = Number(ms)
  if (!Number.isFinite(n) || n <= 0) return '—'
  return n < 1000 ? Math.round(n) + 'ms' : (n / 1000).toFixed(1).replace(/\.0$/, '') + 's'
}

export function formatRate(rate?: number | null): string {
  if (rate == null) return '—'
  const n = Number(rate)
  if (!Number.isFinite(n) || n < 0) return '—'
  return n.toFixed(1) + 'tok/s'
}

/** 用量、积分表里的数：1.2k / 3.45M / 1.20B */
export function fmtTok(n?: number | null): string {
  const v = Number(n || 0)
  if (v >= 1e9) return (v / 1e9).toFixed(2) + 'B'
  if (v >= 1e6) return (v / 1e6).toFixed(2) + 'M'
  if (v >= 1e3) return (v / 1e3).toFixed(1) + 'k'
  return String(v)
}

export function fmtMs(ms?: number | null): string {
  const v = Number(ms || 0)
  if (!v) return '—'
  if (v >= 1000) return (v / 1000).toFixed(2) + 's'
  return Math.round(v) + 'ms'
}

export function fmtRate(r?: number | null): string {
  return r ? Number(r).toFixed(1) + ' tok/s' : '—'
}

/** 上下文、输出上限：128000 → 128K */
export function fmtK(n?: number | null): string {
  const v = Number(n || 0)
  return v >= 1000 ? Math.round(v / 1000) + 'K' : String(v)
}

/** 本地时间 yyyy/MM/dd HH:mm:ss */
export function dateTime(ms: number | null | undefined): string {
  if (ms == null) return '—'
  return new Date(ms).toLocaleString('zh-CN', {
    hour12: false, year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit', second: '2-digit',
  })
}

/** 距离某个将来时刻还有多久：「3 天后」「5 小时后」「12 分钟后」 */
export function until(ms: number, now = Date.now()): string {
  const diff = ms - now
  if (diff <= 0) return '已到期'
  const m = Math.ceil(diff / 60000)
  if (m < 60) return m + ' 分钟后'
  const h = Math.ceil(diff / 3600000)
  if (h < 24) return h + ' 小时后'
  return Math.ceil(diff / 86400000) + ' 天后'
}
