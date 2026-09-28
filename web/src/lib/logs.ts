// 运行日志的频道与级别判定（级别靠关键字，与旧版一致）。
import type { LogEntry } from './types'

export const LOG_CHANNELS = ['all', 'task', 'chat', 'sys'] as const
export type LogChannel = (typeof LOG_CHANNELS)[number]
export const CHANNEL_LABEL: Record<string, string> = { all: '全部', task: '任务', chat: '对话', sys: '系统' }

export const isLogChannel = (v: unknown): v is LogChannel => LOG_CHANNELS.includes(v as LogChannel)

export type LogLevel = 'error' | 'warn' | 'info'
export const LEVEL_FILTERS = ['all', 'warn', 'error'] as const
export type LevelFilter = (typeof LEVEL_FILTERS)[number]

export function logLevel(text: string): LogLevel {
  if (/error|失败|错误/.test(text)) return 'error'
  if (/warn|冷却|熔断/.test(text)) return 'warn'
  return 'info'
}

/** 按频道、级别（警告及以上 / 仅错误）、关键字（不分大小写）筛选 */
export function filterLogs(entries: LogEntry[], ch: LogChannel, level: LevelFilter, q: string): LogEntry[] {
  const kw = q.trim().toLowerCase()
  return entries.filter((e) => {
    if (ch !== 'all' && e.ch !== ch) return false
    if (level !== 'all') {
      const lv = logLevel(e.text)
      if (level === 'error' ? lv !== 'error' : lv === 'info') return false
    }
    return !kw || e.text.toLowerCase().includes(kw)
  })
}
