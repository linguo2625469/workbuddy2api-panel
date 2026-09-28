// 积分构成：一个账号的余额是若干积分包之和。包按来源命名（「国内运营裂变包」「拉新权益包」
// 「个人体验版」…），面额从 6 到 1500 不等，且按次发放。所以两个任务完成度完全一致的账号，
// 余额可能差上千——差别只在包里。这里把逐包明细摊开，并给每个来源一个稳定配色，跨账号对比时同色即同类。
// （纯函数从旧版 app.js 平移，口径不变，见 packages.test.ts）
import type { CreditPackage, PackageAccount } from './types'

// 分类色：8 个固定槽位（CSS 变量，明暗两套，定义和校验说明见 index.css），按顺序分配、不循环复用，
// 第 9 个起一律用「其他」灰——循环复用会让两个不同的账号/来源同色，读者分不清。
export const VIZ_SLOTS = Array.from({ length: 8 }, (_, i) => `var(--viz-${i + 1})`)
export const VIZ_OTHER = 'var(--viz-other)'

export const pkColor = (i: number) => (i >= 0 && i < VIZ_SLOTS.length ? VIZ_SLOTS[i] : VIZ_OTHER)

/** 按 UID 稳定分配账号颜色：排序后分配，账号刷新/重排不会换色 */
export function pkAccountColorMap(list: Pick<PackageAccount, 'uid' | 'error'>[] | null | undefined): Map<string, string> {
  const uids = (list || []).filter((a) => a && !a.error && a.uid).map((a) => String(a.uid)).sort()
  const colors = new Map<string, string>()
  uids.forEach((uid, i) => colors.set(uid, pkColor(i)))
  return colors
}

export interface SourceGroup {
  key: string
  name: string
  code: string
  n: number
  remain: number
  size: number
  used: number
  minEnd: string
  minCreated: string
}

/** 把包按来源归并，得到「来源 → 面额/余额/个数」。两个号的差异一定体现在某几个来源的面额上。 */
export function pkBySource(packs: CreditPackage[]): SourceGroup[] {
  const m = new Map<string, SourceGroup>()
  for (const p of packs) {
    // 分组键用 code + name：上游给「首登赠送」和普通活动包用了同一个 PackageName 和同一个 PackageCode，
    // 只按 name 会把两类混成一类，那正是当初「两个号为何差 1500」看不出来的原因。
    const k = pkSourceKey(p)
    const e = m.get(k) || {
      key: k, name: p.name || '(未命名)', code: p.package_code || '',
      n: 0, remain: 0, size: 0, used: 0, minEnd: '', minCreated: '',
    }
    e.n += 1
    e.remain += Number(p.remain || 0)
    e.size += Number(p.size || 0)
    e.used += Number(p.used || 0)
    const t = (p.end_time || '').slice(0, 10)
    if (t && (!e.minEnd || t < e.minEnd)) e.minEnd = t
    const c = (p.created_at || '').slice(0, 10)
    if (c && (!e.minCreated || c < e.minCreated)) e.minCreated = c
    m.set(k, e)
  }
  return [...m.values()].sort((a, b) => b.size - a.size)
}

export const pkSourceKey = (p: CreditPackage) => (p.package_code || '') + '|' + (p.name || '(未命名)')

export const PK_DEFAULT_DETAIL_LIMIT = 5

export function pkDetailLimitValue(raw: unknown): number {
  const n = Number(raw)
  return Number.isFinite(n) && n > 0 ? Math.floor(n) : PK_DEFAULT_DETAIL_LIMIT
}

export function pkDetailLimit(cfg: { panel?: { package_detail_limit?: unknown } } | null | undefined): number {
  return pkDetailLimitValue(cfg?.panel?.package_detail_limit)
}

export const PK_DAY_MS = 24 * 3600 * 1000

/** 包的到期时刻（毫秒）：优先 expires_at，其次 end_time（无时区按北京时间） */
export function pkExpiryMs(p: Partial<CreditPackage> | null | undefined): number | null {
  const raw = Number(p?.expires_at)
  if (Number.isFinite(raw) && raw > 0) return raw
  const text = String(p?.end_time || '').trim()
  if (!text) return null
  let iso = text.includes('T') ? text : text.replace(' ', 'T')
  if (!/(?:Z|[+-]\d\d:\d\d)$/.test(iso)) iso += '+08:00'
  const parsed = Date.parse(iso)
  return Number.isFinite(parsed) ? parsed : null
}

function pkDetailCompare(a: Partial<CreditPackage>, b: Partial<CreditPackage>): number {
  const sizeOf = (p: Partial<CreditPackage>) => {
    const n = Number(p?.size)
    return Number.isFinite(n) ? n : 0
  }
  const ea = pkExpiryMs(a), eb = pkExpiryMs(b)
  if (ea == null && eb != null) return 1
  if (ea != null && eb == null) return -1
  if (ea != null && eb != null && ea !== eb) return ea - eb
  return sizeOf(b) - sizeOf(a)
}

export interface DetailGroups<P> {
  visible: P[]
  rest: P[]
  used: P[]
  restSize: number
  restRemain: number
  usedSize: number
}

/** 单账号逐包明细：正余额包按到期时间挑出默认展示项，其余正余额包与已用完包分别折叠；同一到期时间按面额降序 */
export function pkDetailGroups<P extends Partial<CreditPackage>>(packs: P[] | null | undefined, limit: unknown): DetailGroups<P> {
  const active: P[] = [], used: P[] = []
  let usedSize = 0, restSize = 0, restRemain = 0
  for (const p of packs || []) {
    if (Number(p?.remain) > 0) {
      active.push(p)
      continue
    }
    used.push(p)
    const size = Number(p?.size)
    if (Number.isFinite(size)) usedSize += size
  }
  active.sort(pkDetailCompare)
  used.sort(pkDetailCompare)
  const visible = active.slice(0, pkDetailLimitValue(limit))
  const rest = active.slice(visible.length)
  for (const p of rest) {
    const size = Number(p?.size)
    if (Number.isFinite(size)) restSize += size
    const remain = Number(p?.remain)
    if (Number.isFinite(remain)) restRemain += remain
  }
  return { visible, rest, used, restSize, restRemain, usedSize }
}

/** 越快到期颜色越深：1 天内不透明度 0.25，30 天及以上 1 */
export function pkCreditOpacity(days: number | null | undefined): number {
  if (days == null || !Number.isFinite(Number(days))) return 1
  return 0.25 + (0.75 * Math.max(0, Math.min(29, Number(days) - 1))) / 29
}

export function pkExpiryText(expiresAt: number | null, now = Date.now()): string {
  if (!expiresAt) return '无到期时间'
  const diff = expiresAt - now
  if (diff <= 0) return '已到期'
  const minutes = Math.max(1, Math.ceil(diff / 60000))
  if (minutes < 60) return '剩余 ' + minutes + ' 分钟'
  const hours = Math.ceil(diff / 3600000)
  if (hours < 24) return '剩余 ' + hours + ' 小时'
  return '剩余 ' + Math.ceil(diff / PK_DAY_MS) + ' 天'
}

export function pkExpiryDateTime(expiresAt: number | null): string {
  if (!expiresAt) return '—'
  return new Date(expiresAt).toLocaleString('zh-CN', {
    timeZone: 'Asia/Shanghai', hour12: false,
    year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit', second: '2-digit',
  })
}

export interface CreditSegment {
  amount: number
  expiresAt: number | null
  days: number | null
  source: string
  uid: string
  accountName: string
}

type SegmentAccount = Pick<PackageAccount, 'uid' | 'remain'> & { nickname?: string; packages?: Partial<CreditPackage>[] | null; error?: string }

/** 账号余额按包拆成到期段：以账号总余额为上限逐包计入（上游偶有重复记录，避免膨胀），按到期先后排序 */
export function pkAccountSegments(a: SegmentAccount, now: number): CreditSegment[] {
  let balance = Math.max(0, Number(a.remain || 0))
  const out: CreditSegment[] = []
  for (const p of a.packages || []) {
    const remain = Number(p.remain || 0)
    if (!Number.isFinite(remain) || remain <= 0 || balance <= 0) continue
    const amount = Math.min(balance, remain)
    const expiresAt = pkExpiryMs(p)
    out.push({
      amount,
      expiresAt,
      days: expiresAt == null ? null : Math.max(0, Math.ceil((expiresAt - now) / PK_DAY_MS)),
      source: p.name || '积分',
      uid: String(a.uid || ''),
      accountName: a.nickname || String(a.uid || '').slice(0, 8) || '未命名账号',
    })
    balance -= amount
  }
  return out.sort((x, y) => {
    if (x.expiresAt == null && y.expiresAt != null) return 1
    if (x.expiresAt != null && y.expiresAt == null) return -1
    return (x.expiresAt || 0) - (y.expiresAt || 0)
  })
}

export interface CreditDayRow {
  days: number
  credits: number
  segments: CreditSegment[]
}

/** 按精确剩余天数逐行聚合（对齐 WorkDaddy）：无有效到期时间的余额不进图表，也不猜测到期日 */
export function summarizeCreditDays(list: SegmentAccount[] | null | undefined, now: number) {
  const buckets = new Map<number, CreditDayRow>()
  let unavailable = 0
  for (const a of list || []) {
    if (a.error || !Number.isFinite(Number(a.remain))) {
      unavailable++
      continue
    }
    for (const segment of pkAccountSegments(a, now)) {
      if (segment.days == null) continue
      let row = buckets.get(segment.days)
      if (!row) {
        row = { days: segment.days, credits: 0, segments: [] }
        buckets.set(segment.days, row)
      }
      row.credits += segment.amount
      row.segments.push(segment)
    }
  }
  const rows = [...buckets.values()].sort((a, b) => a.days - b.days)
  for (const row of rows) {
    row.segments.sort((a, b) =>
      (a.expiresAt || Infinity) - (b.expiresAt || Infinity) ||
      a.accountName.localeCompare(b.accountName) ||
      a.source.localeCompare(b.source))
  }
  return { rows, accountCount: (list || []).length, unavailable }
}
