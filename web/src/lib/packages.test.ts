// 从 Go 端 internal/panel/frontend_test.go 平移过来的两个用例（断言值原样保留）。
import { describe, expect, it } from 'vitest'
import { pkAccountColorMap, pkDetailGroups, pkDetailLimit, summarizeCreditDays } from './packages'

describe('pkDetailGroups', () => {
  it('同到期时间按面额降序；其余未用完包与零/负余额包分别聚合', () => {
    const input = [
      { id: 'small-late', size: 100, remain: 1, expires_at: 400 },
      { id: 'zero-early-b', size: 200, remain: 0, expires_at: 200 },
      { id: 'small-early', size: 100, remain: 2, expires_at: 200 },
      { id: 'large-unknown', size: 300, remain: 3, end_time: '' },
      { id: 'zero-early-a', size: 200, remain: -1, expires_at: 200 },
      { id: 'small-unknown', size: 100, remain: 1, end_time: '' },
      { id: 'large-early', size: 300, remain: 4, expires_at: 200 },
      { id: 'zero-late', size: 300, remain: 0, expires_at: 300 },
    ]
    const before = input.map((p) => p.id).join(',')
    const out = pkDetailGroups(input, 2)
    expect(out.visible.map((p) => p.id)).toEqual(['large-early', 'small-early'])
    expect(out.rest.map((p) => p.id)).toEqual(['small-late', 'large-unknown', 'small-unknown'])
    expect(out.used.map((p) => p.id)).toEqual(['zero-early-b', 'zero-early-a', 'zero-late'])
    expect(out.restSize).toBe(500)
    expect(out.restRemain).toBe(5)
    expect(out.usedSize).toBe(700)
    expect(pkDetailLimit({})).toBe(5)
    expect(pkDetailLimit({ panel: { package_detail_limit: 7 } })).toBe(7)
    // 不改动入参顺序
    expect(input.map((p) => p.id).join(',')).toBe(before)
  })
})

describe('summarizeCreditDays', () => {
  it('精确剩余天数聚合、账号内按总余额钳制、无到期批次不进入图表', () => {
    const day = 86400000, now = 100000
    const out = summarizeCreditDays([
      { uid: 'a', remain: 100, packages: [
        { name: 'soon-a', remain: 30, expires_at: now + day },
        { name: 'later', remain: 70, expires_at: now + 7 * day },
      ] },
      { uid: 'b', remain: 55, packages: [
        { name: 'soon-b', remain: 20, expires_at: now + day },
        { name: 'unknown', remain: 5, end_time: '' },
      ] },
      { uid: 'err', remain: 0, error: 'offline' },
    ], now)
    expect(out.rows.map((r) => ({ days: r.days, credits: r.credits }))).toEqual([
      { days: 1, credits: 50 },
      { days: 7, credits: 70 },
    ])
    expect(out.accountCount).toBe(3)
    expect(out.unavailable).toBe(1)
    expect(pkAccountColorMap([{ uid: 'b' }, { uid: 'a' }]).get('a')).toBe('var(--viz-1)')
    expect(pkAccountColorMap([{ uid: 'a' }, { uid: 'b' }]).get('b')).toBe('var(--viz-2)')
  })
})

describe('分类色', () => {
  it('按 UID 排序占用 8 个固定槽位，第 9 个起归入「其他」，不循环复用', () => {
    const list = Array.from({ length: 10 }, (_, i) => ({ uid: 'u' + i }))
    const colors = pkAccountColorMap(list)
    expect(colors.get('u0')).toBe('var(--viz-1)')
    expect(colors.get('u7')).toBe('var(--viz-8)')
    expect(colors.get('u8')).toBe('var(--viz-other)')
    expect(colors.get('u9')).toBe('var(--viz-other)')
    expect(new Set([...colors.values()].filter((c) => c !== 'var(--viz-other)')).size).toBe(8)
    // 查询失败的账号不占槽位
    expect(pkAccountColorMap([{ uid: 'x', error: 'offline' }, { uid: 'y' }]).get('y')).toBe('var(--viz-1)')
  })
})
