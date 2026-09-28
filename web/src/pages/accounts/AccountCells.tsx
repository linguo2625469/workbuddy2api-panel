// 账号的各个展示单元（名称、状态、积分、用量），账号表、手机卡片、详情抽屉共用。
import { Chip, ProgressBar } from '@heroui/react'
import { activeRateLimits, creditPercent, type Health } from '../../lib/accounts'
import { dur, formatLatency, formatRate, formatTokenCount, fmtTok, timeMs, until } from '../../lib/format'
import type { Account } from '../../lib/types'

export function AccountName({ a }: { a: Account }) {
  return (
    <div className="min-w-0">
      <div className="flex items-center gap-1.5">
        <span className={`truncate font-medium ${a.nickname ? '' : 'text-muted'}`}>{a.nickname || '未命名'}</span>
        {a.realm === 'global' && <Chip size="sm" variant="soft" color="accent">国际版</Chip>}
      </div>
      <div className="truncate font-mono text-xs text-muted">{a.uid.length > 16 ? a.uid.slice(0, 16) + '…' : a.uid}</div>
    </div>
  )
}

export function HealthChip({ health, now }: { health: Health; now: number }) {
  if (health.state === 'disabled') return <Chip size="sm" variant="soft" color="danger">已禁用</Chip>
  if (health.state === 'cooling') {
    return <Chip size="sm" variant="soft" color="warning">{health.kind} · {dur(((health.coolEnd ?? now) - now) / 1000)}</Chip>
  }
  return <Chip size="sm" variant="soft" color="success">可用</Chip>
}

export function StatusCell({ a, health, now }: { a: Account; health: Health; now: number }) {
  const limited = activeRateLimits(a, now)
  return (
    <div className="flex min-w-40 flex-col items-start gap-1">
      <div className="flex flex-wrap items-center gap-1">
        <HealthChip health={health} now={now} />
        {limited.length > 0 && (
          <Chip size="sm" variant="soft" color="warning">{limited.length} 个模型限额中</Chip>
        )}
      </div>
      {a.reason && <span className="line-clamp-2 max-w-40 text-xs text-muted">{a.reason}</span>}
    </div>
  )
}

/** 最早到期的一批积分：「3 天后到期 120 分」（路由按最早到期优先选号，这里能看出为什么选它） */
export function EarliestExpiry({ a, now }: { a: Account; now: number }) {
  const at = timeMs(a.credits_earliest_expiry)
  if (at == null || at <= now || !a.credits_earliest_remaining) return null
  return <span className="text-xs text-muted">{until(at, now)}到期 {fmtTok(a.credits_earliest_remaining)} 分</span>
}

export function CreditsCell({ a, poolMax, now }: { a: Account; poolMax: number; now: number }) {
  if (a.credits == null) return <span className="text-muted">—</span>
  const pct = creditPercent(a, poolMax)
  return (
    <div className="flex min-w-28 flex-col gap-1">
      <span className="font-mono text-sm tabular-nums">
        {a.credits}
        {!!a.credits_total && <span className="text-xs text-muted">/{a.credits_total}</span>}
      </span>
      <ProgressBar aria-label={a.credits_total ? `剩余 ${pct}%` : '积分（相对池内最高）'} value={pct} size="sm" className="max-w-28">
        <ProgressBar.Track><ProgressBar.Fill /></ProgressBar.Track>
      </ProgressBar>
      <EarliestExpiry a={a} now={now} />
    </div>
  )
}

/** 最近一次请求的用量：次数 / token / 延迟 / 速率 */
export function UsageChips({ a }: { a: Account }) {
  const tu = a.token_usage || {}
  const total = formatTokenCount(tu.total_tokens)
  return (
    <div className="flex flex-wrap gap-1">
      <Chip size="sm" variant="soft" color="accent">{tu.request_count || 0} 次</Chip>
      <Chip size="sm" variant="soft" color="accent">{total}{total !== '—' && ' tok'}</Chip>
      <Chip size="sm" variant="soft" color="warning">{formatLatency(tu.last_latency_ms)}</Chip>
      <Chip size="sm" variant="soft" color="success">{formatRate(tu.last_tokens_per_second)}</Chip>
    </div>
  )
}
