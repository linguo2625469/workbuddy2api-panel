// 账号详情抽屉：展示后端已有、账号表放不下的运维信息——模型级限额台账、成本台账、
// 最早到期批次、连败 / 熔断计数、最近一次请求。数据取自总览（随 5 秒轮询实时更新）。
import type { ReactNode } from 'react'
import { Chip, Drawer, Separator, useMediaQuery } from '@heroui/react'
import { activeRateLimits, type Health } from '../../lib/accounts'
import { ago, dateTime, formatLatency, formatRate, formatTokenCount, fmtTok, timeMs, until } from '../../lib/format'
import type { Account } from '../../lib/types'
import { AccountButtons } from './AccountButtons'
import { HealthChip } from './AccountCells'
import type { AccountAction } from './useAccountActions'

function Row({ label, children }: { label: ReactNode; children: ReactNode }) {
  return (
    <div className="flex items-start justify-between gap-4 py-1.5 text-sm">
      <span className="shrink-0 text-muted">{label}</span>
      <span className="min-w-0 text-right tabular-nums">{children}</span>
    </div>
  )
}

function Section({ title, children }: { title: ReactNode; children: ReactNode }) {
  return (
    <section className="flex flex-col">
      <h3 className="mb-1 text-sm font-semibold">{title}</h3>
      {children}
    </section>
  )
}

const when = (iso?: string) => {
  const t = timeMs(iso)
  return t == null ? '—' : dateTime(t)
}

export function AccountDrawer({ account: a, health, now, busy, run, onClose }: {
  account: Account | null
  health: Health | null
  now: number
  busy: string | null
  run: (a: Account, action: AccountAction) => void
  onClose: () => void
}) {
  const isWide = useMediaQuery('(min-width: 640px)')
  return (
    <Drawer.Backdrop isOpen={!!a} onOpenChange={(v) => { if (!v) onClose() }}>
      <Drawer.Content placement={isWide ? 'right' : 'bottom'}>
        <Drawer.Dialog className={isWide ? 'w-[440px] max-w-full' : 'max-h-[88dvh]'}>
          {isWide ? <Drawer.CloseTrigger /> : <Drawer.Handle />}
          {a && health && <Body a={a} health={health} now={now} busy={busy} run={run} />}
        </Drawer.Dialog>
      </Drawer.Content>
    </Drawer.Backdrop>
  )
}

function Body({ a, health, now, busy, run }: {
  a: Account
  health: Health
  now: number
  busy: string | null
  run: (a: Account, action: AccountAction) => void
}) {
  const limited = activeRateLimits(a, now)
  const costs = (a.model_costs || []).filter((c) => c.model)
  const tu = a.token_usage || {}
  const earliest = timeMs(a.credits_earliest_expiry)
  const breaker = timeMs(a.breaker_until)
  const degrade = timeMs(a.degrade_until)

  return (
    <>
      <Drawer.Header>
        <Drawer.Heading className="flex flex-wrap items-center gap-2">
          {a.nickname || '未命名'}
          {a.realm === 'global' && <Chip size="sm" variant="soft" color="accent">国际版</Chip>}
        </Drawer.Heading>
        <p className="select-all break-all font-mono text-xs text-muted">{a.uid}</p>
      </Drawer.Header>
      <Drawer.Body className="flex flex-col gap-5">
        <Section title="状态">
          <Row label="当前"><HealthChip health={health} now={now} /></Row>
          {health.coolEnd != null && <Row label="恢复时间">{dateTime(health.coolEnd)}</Row>}
          {a.reason && <Row label={a.disabled ? '禁用原因' : '原因'}><span className="break-all">{a.reason}</span></Row>}
          <Row label="连续失败">{a.consecutive_fails}{degrade != null && degrade > now && <span className="text-warning">（降权至 {dateTime(degrade)}）</span>}</Row>
          <Row label="熔断计数">{a.breaker_fails}{breaker != null && breaker > now && <span className="text-warning">（熔断至 {dateTime(breaker)}）</span>}</Row>
          {!!a.soft_streak && <Row label="连续软冷却">{a.soft_streak} 次（退避指数）</Row>}
          <Row label="在途请求">{a.in_flight}</Row>
        </Section>

        <Separator />
        <Section title={`模型级限额${limited.length ? `（${limited.length}）` : ''}`}>
          {limited.length === 0 ? (
            <p className="text-sm text-muted">没有模型在限额中。</p>
          ) : (
            <ul className="flex flex-col gap-2">
              {limited.map((m) => {
                const u = timeMs(m.until), r = timeMs(m.reset_at)
                return (
                  <li key={m.model} className="rounded-xl bg-surface-secondary px-3 py-2 text-sm">
                    <div className="flex items-center justify-between gap-2">
                      <span className="truncate font-medium">{m.model}</span>
                      {u != null && <Chip size="sm" variant="soft" color="warning">{until(u, now)}恢复</Chip>}
                    </div>
                    {u != null && <div className="text-xs text-muted">冷却到 {dateTime(u)}</div>}
                    {r != null && <div className="text-xs text-muted">上游称 {dateTime(r)} 重置</div>}
                    {m.reason && <div className="mt-0.5 break-all text-xs text-muted">{m.reason}</div>}
                  </li>
                )
              })}
            </ul>
          )}
        </Section>

        <Separator />
        <Section title="积分">
          <Row label="剩余 / 总额">{a.credits ?? '—'}{a.credits_total ? ' / ' + a.credits_total : ''}</Row>
          {earliest != null && earliest > now && (
            <Row label="最早到期">{fmtTok(a.credits_earliest_remaining)} 分 · {until(earliest, now)}（{dateTime(earliest)}）</Row>
          )}
          {!!a.credits_expiring && <Row label="快过期窗口内">{fmtTok(a.credits_expiring)} 分</Row>}
        </Section>

        <Separator />
        <Section title="成本台账">
          {costs.length === 0 ? (
            <p className="text-sm text-muted">暂无实测单价（有效期内没有观测）。</p>
          ) : (
            costs.map((c) => (
              <Row key={c.model} label={<span className="break-all">{c.model}</span>}>
                {c.cost_per_1k <= 0 ? <span className="text-success">免费</span> : c.cost_per_1k + ' credits/1K'}
                <span className="block text-xs text-muted">{c.samples ? c.samples + ' 次样本 · ' : ''}{ago(c.last_seen, now)}</span>
              </Row>
            ))
          )}
        </Section>

        <Separator />
        <Section title="请求">
          <Row label="成功 / 失败">{a.success_count || 0} / <span className="text-danger">{a.err_total || 0}</span></Row>
          <Row label="最近成功">{ago(a.last_success, now)}</Row>
          <Row label="最近失败">{ago(a.last_err, now)}</Row>
          <Row label="累计请求">{tu.request_count || 0} 次</Row>
          <Row label="累计 token">{formatTokenCount(tu.total_tokens)}（输入 {formatTokenCount(tu.prompt_tokens)} · 输出 {formatTokenCount(tu.completion_tokens)}）</Row>
          <Row label="最近一次">{tu.last_model || '—'} · {formatLatency(tu.last_latency_ms)} · {formatRate(tu.last_tokens_per_second)}</Row>
          <Row label="最近使用">{when(tu.last_used_at)}</Row>
        </Section>
      </Drawer.Body>
      <Drawer.Footer>
        <AccountButtons a={a} frozen={health.state !== 'ok'} busy={busy} run={run} />
      </Drawer.Footer>
    </>
  )
}
