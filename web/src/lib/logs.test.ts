import { describe, expect, it } from 'vitest'
import { filterLogs, logLevel } from './logs'

const entries = [
  { ts: '', ch: 'task', text: '签到完成 uid=a' },
  { ts: '', ch: 'task', text: '签到失败 uid=b' },
  { ts: '', ch: 'chat', text: '限流冷却 600s' },
  { ts: '', ch: 'sys', text: 'Panel Started' },
]

describe('logs', () => {
  it('按关键字判定级别', () => {
    expect(logLevel('upstream error 502')).toBe('error')
    expect(logLevel('熔断 30m')).toBe('warn')
    expect(logLevel('ok')).toBe('info')
  })

  it('频道、级别、关键字组合筛选', () => {
    expect(filterLogs(entries, 'task', 'all', '').length).toBe(2)
    expect(filterLogs(entries, 'all', 'warn', '').map((e) => e.text)).toEqual(['签到失败 uid=b', '限流冷却 600s'])
    expect(filterLogs(entries, 'all', 'error', '').map((e) => e.text)).toEqual(['签到失败 uid=b'])
    expect(filterLogs(entries, 'all', 'all', 'panel').map((e) => e.ch)).toEqual(['sys'])
  })
})
