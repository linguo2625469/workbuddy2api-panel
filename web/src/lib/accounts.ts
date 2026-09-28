// 账号池的状态推导、筛选与排序（纯函数，见 accounts.test.ts）。
import { timeMs } from './format'
import type { Account } from './types'

export type AccountState = 'ok' | 'cooling' | 'disabled'
export type StatusFilter = 'all' | AccountState

export interface Health {
  state: AccountState
  /** 冷却截止时刻（毫秒）；未冷却为 null */
  coolEnd: number | null
  /** 冷却类型：熔断 / 连败降权 / 积分冷却 / 限流冷却 */
  kind: string
}

/**
 * 账号健康：禁用优先；否则取「账号级冷却、熔断、连败降权」三者最晚的截止时刻。
 * cool_remaining_sec 是接口返回时刻的剩余秒数，用 fetchedAt 换算成绝对时刻，页面才能逐秒倒数。
 */
export function accountHealth(a: Account, fetchedAt: number): Health {
  if (a.disabled) return { state: 'disabled', coolEnd: null, kind: '已禁用' }
  const soft = a.cool_remaining_sec ? fetchedAt + a.cool_remaining_sec * 1000 : 0
  const breaker = timeMs(a.breaker_until) ?? 0
  const degrade = timeMs(a.degrade_until) ?? 0
  const end = Math.max(soft, breaker, degrade)
  if (end <= fetchedAt) return { state: 'ok', coolEnd: null, kind: '可用' }
  const kind = breaker > Math.max(soft, degrade) ? '熔断'
    : degrade > soft ? '连败降权'
    : a.cool_kind === 'hard_credit' ? '积分冷却' : '限流冷却'
  return { state: 'cooling', coolEnd: end, kind }
}

/** 仍在限额中的模型（到期的自然消失） */
export function activeRateLimits(a: Account, now: number) {
  return (a.rate_limited_models || []).filter((m) => (timeMs(m.until) ?? Infinity) > now)
}

/** 积分进度百分比：有总额按 剩余/总额；旧数据没有总额时按池内最高 = 100% */
export function creditPercent(a: Account, poolMax: number): number {
  const c = a.credits || 0
  if (a.credits_total && a.credits_total > 0) return Math.min(100, Math.round((c / a.credits_total) * 100))
  return Math.round((c / Math.max(1, poolMax)) * 100)
}

export type AccountSortKey = 'name' | 'credits' | 'errors' | 'last_success'

export function filterAccounts(list: Account[], fetchedAt: number, status: StatusFilter, q: string): Account[] {
  const kw = q.trim().toLowerCase()
  return list.filter((a) => {
    if (status !== 'all' && accountHealth(a, fetchedAt).state !== status) return false
    return !kw || a.uid.toLowerCase().includes(kw) || (a.nickname || '').toLowerCase().includes(kw)
  })
}

export function sortAccounts(list: Account[], key: AccountSortKey | null, desc: boolean): Account[] {
  if (!key) return list
  const val = (a: Account): number | string => {
    switch (key) {
      case 'name': return (a.nickname || a.uid).toLowerCase()
      case 'credits': return a.credits ?? -1
      case 'errors': return a.err_total || 0
      case 'last_success': return timeMs(a.last_success) ?? 0
    }
  }
  const out = [...list].sort((x, y) => {
    const a = val(x), b = val(y)
    return a < b ? -1 : a > b ? 1 : 0
  })
  return desc ? out.reverse() : out
}
