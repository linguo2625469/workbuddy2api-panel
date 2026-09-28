import { describe, expect, it } from 'vitest'
import { accountHealth, activeRateLimits, creditPercent, filterAccounts, sortAccounts } from './accounts'
import type { Account } from './types'

const base: Account = {
  uid: 'u1', credits: 50, cooling: false, disabled: false, consecutive_fails: 0, in_flight: 0, breaker_fails: 0,
}
const now = Date.parse('2026-09-28T12:00:00Z')
const iso = (ms: number) => new Date(ms).toISOString()

describe('accountHealth', () => {
  it('禁用优先于冷却', () => {
    expect(accountHealth({ ...base, disabled: true, cool_remaining_sec: 60 }, now).state).toBe('disabled')
  })
  it('零值时间不算冷却', () => {
    expect(accountHealth({ ...base, breaker_until: '0001-01-01T00:00:00Z' }, now)).toEqual({ state: 'ok', coolEnd: null, kind: '可用' })
  })
  it('取三种冷却里最晚的截止时刻，并按来源命名', () => {
    expect(accountHealth({ ...base, cool_remaining_sec: 60 }, now)).toEqual({ state: 'cooling', coolEnd: now + 60_000, kind: '限流冷却' })
    expect(accountHealth({ ...base, cool_remaining_sec: 60, cool_kind: 'hard_credit' }, now).kind).toBe('积分冷却')
    expect(accountHealth({ ...base, cool_remaining_sec: 60, breaker_until: iso(now + 120_000) }, now).kind).toBe('熔断')
    expect(accountHealth({ ...base, cool_remaining_sec: 60, degrade_until: iso(now + 90_000) }, now).kind).toBe('连败降权')
  })
})

describe('账号表辅助', () => {
  it('只保留仍在限额中的模型', () => {
    const a = { ...base, rate_limited_models: [{ model: 'x', until: iso(now - 1) }, { model: 'y', until: iso(now + 1000) }] }
    expect(activeRateLimits(a, now).map((m) => m.model)).toEqual(['y'])
  })
  it('积分百分比：有总额用总额，没有按池内最高', () => {
    expect(creditPercent({ ...base, credits: 25, credits_total: 100 }, 999)).toBe(25)
    expect(creditPercent({ ...base, credits: 25 }, 50)).toBe(50)
  })
  it('按状态和关键字筛选、按列排序', () => {
    const list = [
      { ...base, uid: 'aaa', nickname: '甲', credits: 10, err_total: 3 },
      { ...base, uid: 'bbb', nickname: '乙', credits: 30, disabled: true },
      { ...base, uid: 'ccc', credits: 20, cool_remaining_sec: 30 },
    ]
    expect(filterAccounts(list, now, 'cooling', '').map((a) => a.uid)).toEqual(['ccc'])
    expect(filterAccounts(list, now, 'all', '乙').map((a) => a.uid)).toEqual(['bbb'])
    expect(sortAccounts(list, 'credits', true).map((a) => a.uid)).toEqual(['bbb', 'ccc', 'aaa'])
    expect(sortAccounts(list, null, false)).toBe(list)
  })
})
