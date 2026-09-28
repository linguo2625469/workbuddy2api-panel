import { describe, expect, it } from 'vitest'
import { ALL_FIELDS, changedFields, fromForm, toForm, validateField } from './config'

const field = (name: string) => ALL_FIELDS.find((f) => f.name === name)!

describe('配置表单', () => {
  it('回填与提交互为逆运算：数组变「9, 21」，空输入不提交，开关总是提交', () => {
    const cfg = {
      listen: ':7863',
      schedule: { checkin_hours: [9, 21], checkin_enabled: true },
      pool: { max_in_flight: 3 },
      prompt: { mode: 'custom' },
    }
    const form = toForm(cfg)
    expect(form.checkin_hours).toBe('9, 21')
    expect(form.max_in_flight).toBe('3')
    expect(form.api_key).toBe('')
    expect(form.prefer_expiring).toBe(false)
    const body = fromForm(form) as Record<string, Record<string, unknown>>
    expect(body.listen).toBe(':7863')
    expect(body.schedule.checkin_hours).toEqual([9, 21])
    expect(body.pool.max_in_flight).toBe(3)
    expect('api_key' in body).toBe(false)
    expect(body.pool.prefer_expiring).toBe(false)
  })

  it('时长、时点、数字校验', () => {
    expect(validateField(field('soft_rate'), '1h30m')).toBeNull()
    expect(validateField(field('soft_rate'), '30 分钟')).not.toBeNull()
    expect(validateField(field('soft_rate'), '')).toBeNull()
    expect(validateField(field('checkin_hours'), '9，21')).toBeNull()
    expect(validateField(field('checkin_hours'), '9, 24')).not.toBeNull()
    expect(validateField(field('max_in_flight_global'), '0')).not.toBeNull()
  })

  it('统计改动项（忽略首尾空格）', () => {
    const a = toForm({ listen: ':7863' })
    expect(changedFields(a, { ...a, listen: ' :7863 ' })).toEqual([])
    expect(changedFields(a, { ...a, listen: ':8000', prefer_expiring: true })).toEqual(['listen', 'prefer_expiring'])
  })
})
