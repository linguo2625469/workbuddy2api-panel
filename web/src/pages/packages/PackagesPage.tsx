// 积分构成：积分到期分布（按剩余天数、账号分色）、账号对比卡片、单账号逐包明细。
// 数据逐账号实时查上游（慢），一分钟内切回来直接用缓存，页面上有「刷新」。
import { useMemo } from 'react'
import { Accordion, Card, Table } from '@heroui/react'
import { Bar, BarChart, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts'
import { ChevronDown } from '@gravity-ui/icons'
import { fmtTok } from '../../lib/format'
import type { TipProps } from '../../lib/chart'
import {
  pkAccountColorMap, pkAccountSegments, pkBySource, pkColor, pkCreditOpacity, pkDetailGroups, pkDetailLimit,
  pkDetailLimitValue, pkExpiryDateTime, pkExpiryText, pkSourceKey, summarizeCreditDays, VIZ_OTHER, type CreditDayRow,
} from '../../lib/packages'
import { useConfig, usePackages } from '../../lib/queries'
import type { CreditPackage, PackageAccount } from '../../lib/types'
import { BusyButton, Empty, Loaded } from '../../components/Feedback'
import { Panel } from '../../components/Panel'

export function PackagesPage() {
  const query = usePackages()
  const cfg = useConfig()
  const limit = pkDetailLimit(cfg.data?.config)
  const list = useMemo(() => query.data?.accounts || [], [query.data])
  const colorOf = useMemo(() => sourceColors(list), [list])
  return (
    <>
      <Panel title="积分到期分布" desc="按批次到期日聚合，颜色区分账号">
        <Loaded query={query} rows={4}>{() => <ExpiryDistribution list={list} now={query.dataUpdatedAt} />}</Loaded>
      </Panel>
      <Panel
        title="账号对比"
        desc={query.data ? list.length + ' 个账号 · 实时查询上游' : '逐账号向上游实时查询'}
        actions={<BusyButton size="sm" variant="secondary" busy={query.isFetching} onPress={() => void query.refetch()}>刷新</BusyButton>}
      >
        <Loaded query={query} rows={3}>{() => <Compare list={list} now={query.dataUpdatedAt} />}</Loaded>
      </Panel>
      {query.data && list.filter((a) => !a.error).map((a) => <Detail key={a.uid} a={a} limit={limit} colorOf={colorOf} />)}
    </>
  )
}

/** 包来源 → 稳定色（按各账号里该来源的最大面额排序，跨账号同色即同类） */
function sourceColors(list: PackageAccount[]) {
  const size = new Map<string, number>()
  for (const a of list) for (const s of pkBySource(a.packages || [])) size.set(s.key, Math.max(size.get(s.key) ?? 0, s.size))
  const keys = [...size.keys()].sort((x, y) => size.get(y)! - size.get(x)!)
  return (key: string) => pkColor(keys.indexOf(key))
}

interface ExpiryRow { label: string; total: string; days: number; row: CreditDayRow; [uid: string]: unknown }

function ExpiryTip({ active, payload }: TipProps) {
  const r = payload?.[0]?.payload as ExpiryRow | undefined
  if (!active || !r) return null
  const segs = r.row.segments
  return (
    <div className="max-w-80 rounded-xl border border-separator bg-overlay px-3 py-2 text-xs shadow-lg">
      <div className="mb-1 font-medium">{r.label} · 共 {r.total} 积分</div>
      {segs.slice(0, 8).map((s, i) => (
        <div key={i} className="py-0.5">
          <span className="font-medium">{s.accountName}</span> · {s.source} · <b className="tabular-nums">{fmtTok(s.amount)}</b>
          <div className="text-muted">到期 {pkExpiryDateTime(s.expiresAt)}（{pkExpiryText(s.expiresAt)}）</div>
        </div>
      ))}
      {segs.length > 8 && <div className="text-muted">…另有 {segs.length - 8} 段</div>}
    </div>
  )
}

// now 取数据拉取时刻：剩余天数按查询那一刻算，与旧版一致
function ExpiryDistribution({ list, now }: { list: PackageAccount[]; now: number }) {
  const summary = useMemo(() => summarizeCreditDays(list, now), [list, now])
  const colors = useMemo(() => pkAccountColorMap(list), [list])
  const uids = [...colors.keys()].filter((uid) => summary.rows.some((r) => r.segments.some((s) => s.uid === uid)))
  const data: ExpiryRow[] = summary.rows.map((row) => {
    const d: ExpiryRow = { label: row.days === 0 ? '已到期' : row.days + ' 天', total: fmtTok(row.credits), days: row.days, row }
    for (const s of row.segments) d[s.uid] = ((d[s.uid] as number | undefined) ?? 0) + s.amount
    return d
  })
  // 图例：前 8 个账号各占一色；第 9 个起同为「其他」灰，合并成一项（名字在悬停明细里）
  const inChart = list.filter((a) => uids.includes(a.uid))
  const legend = inChart.filter((a) => colors.get(a.uid) !== VIZ_OTHER)
  const others = inChart.length - legend.length
  const foot = summary.accountCount + ' 个账号' + (summary.unavailable ? ' · ' + summary.unavailable + ' 个未获取余额' : '')
  if (!data.length) return <><Empty title="暂无可汇总积分" /><p className="text-xs text-muted">{foot}</p></>
  const tick = { fill: 'var(--muted)', fontSize: 11 }
  return (
    <div className="flex flex-col gap-2">
      <div className="flex text-xs text-muted">
        <span className="w-16 shrink-0">剩余天数</span>
        <span className="flex-1 text-center">各账号该批剩余</span>
        <span className="w-16 shrink-0 text-right">剩余积分</span>
      </div>
      <div className="max-h-72 overflow-y-auto overscroll-contain">
        <ResponsiveContainer width="100%" height={data.length * 30 + 8}>
          <BarChart data={data} layout="vertical" margin={{ top: 4, right: 0, bottom: 4, left: 0 }} barCategoryGap={9}>
            <XAxis type="number" hide />
            <YAxis yAxisId="days" type="category" dataKey="label" width={64} tick={tick} tickLine={false} axisLine={false} />
            <YAxis yAxisId="total" type="category" dataKey="total" orientation="right" width={64} tick={{ ...tick, fontFamily: 'ui-monospace, monospace' }} tickLine={false} axisLine={false} />
            <Tooltip content={ExpiryTip} cursor={{ fill: 'var(--surface-secondary)' }} />
            {uids.map((uid) => (
              // 不按剩余天数调透明度：天数已由行标签表示，再淡化会让账号色（按不透明校验过）在快到期的几行里分不清
              <Bar key={uid} yAxisId="days" dataKey={uid} stackId="c" fill={colors.get(uid)} stroke="var(--surface)" strokeWidth={1} isAnimationActive={false} />
            ))}
          </BarChart>
        </ResponsiveContainer>
      </div>
      <div className="flex flex-wrap gap-x-3 gap-y-1 text-xs text-muted">
        {legend.map((a) => (
          <span key={a.uid} className="flex items-center gap-1.5">
            <span className="size-2.5 rounded-sm" style={{ background: colors.get(a.uid) }} />{a.nickname || a.uid.slice(0, 8)}
          </span>
        ))}
        {others > 0 && (
          <span className="flex items-center gap-1.5">
            <span className="size-2.5 rounded-sm" style={{ background: VIZ_OTHER }} />其他 {others} 个账号
          </span>
        )}
      </div>
      <p className="border-t border-separator pt-2 text-xs text-muted">{foot}</p>
    </div>
  )
}

/** 细条：各段按占比铺满（颜色 + 不透明度由调用方给），段间留 2px 缝 */
function MiniBar({ parts, label }: { parts: { key: string; width: number; color: string; opacity?: number; title: string }[]; label: string }) {
  return (
    <div role="img" aria-label={label} className="flex h-2 gap-0.5 overflow-hidden rounded-full bg-surface-secondary">
      {parts.map((p) => (
        <span key={p.key} title={p.title} className="h-full min-w-0.5" style={{ flex: `${p.width} 1 0`, background: p.color, opacity: p.opacity }} />
      ))}
    </div>
  )
}

function Compare({ list, now }: { list: PackageAccount[]; now: number }) {
  const colorOf = useMemo(() => sourceColors(list), [list])
  const accountColors = useMemo(() => pkAccountColorMap(list), [list])
  if (!list.length) return <Empty title="没有账号" />
  const maxRemain = Math.max(1, ...list.map((a) => Number(a.remain || 0)))
  return (
    <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
      {list.map((a) => {
        const name = a.nickname || a.uid.slice(0, 8)
        if (a.error) {
          return (
            <Card key={a.uid} variant="secondary" className="gap-1 p-4">
              <span className="font-medium">{name} <span className="font-mono text-xs text-muted">{a.realm}</span></span>
              <span className="text-sm text-warning">查询失败：{a.error}</span>
            </Card>
          )
        }
        const srcs = pkBySource(a.packages || [])
        const total = Math.max(1, Number(a.size || 0))
        const expiry = pkAccountSegments(a, now)
        const expiryTotal = Math.max(1, expiry.reduce((s, x) => s + x.amount, 0))
        return (
          <Card key={a.uid} variant="secondary" className="gap-2 p-4">
            <span className="font-medium">{name} <span className="font-mono text-xs text-muted">{a.realm}</span></span>
            <span className="font-mono text-2xl font-semibold tabular-nums">{fmtTok(a.remain)}</span>
            <span className="text-xs text-muted">
              共 {fmtTok(a.size)} · {(a.packages || []).length} 个包 · 占最高 {((Number(a.remain || 0) / maxRemain) * 100).toFixed(0)}%
            </span>
            <MiniBar label="积分来源构成" parts={srcs.map((s) => ({ key: s.key, width: s.size / total, color: colorOf(s.key), title: s.name + ' ' + fmtTok(s.size) }))} />
            {expiry.length > 0 && (
              <MiniBar
                label="积分到期分布"
                parts={expiry.map((s, i) => ({
                  key: String(i), width: s.amount / expiryTotal, color: accountColors.get(a.uid) ?? 'var(--accent)', opacity: pkCreditOpacity(s.days),
                  title: s.source + ' ' + fmtTok(s.amount) + ' 积分，到期 ' + pkExpiryDateTime(s.expiresAt) + '（' + pkExpiryText(s.expiresAt) + '）',
                }))}
              />
            )}
            <div className="flex flex-wrap gap-x-3 gap-y-1 text-xs text-muted">
              {srcs.map((s) => (
                <span key={s.key} className="flex items-center gap-1.5">
                  <span className="size-2.5 rounded-sm" style={{ background: colorOf(s.key) }} />
                  {s.name.replace(/^CodeBuddy/, '')} ×{s.n} · {fmtTok(s.size)}{s.minCreated ? ' · 首发 ' + s.minCreated.slice(5) : ''}
                </span>
              ))}
            </div>
          </Card>
        )
      })}
    </div>
  )
}

function PackTable({ packs, colorOf, label }: { packs: CreditPackage[]; colorOf: (key: string) => string; label: string }) {
  return (
    <Table variant="secondary">
      <Table.ScrollContainer>
        <Table.Content aria-label={label} className="min-w-[640px]">
          <Table.Header>
            <Table.Column isRowHeader>包名 / 来源</Table.Column>
            <Table.Column className="text-right">面额</Table.Column>
            <Table.Column className="text-right">剩余</Table.Column>
            <Table.Column className="text-right">已用</Table.Column>
            <Table.Column className="text-right">发放</Table.Column>
            <Table.Column className="text-right">到期</Table.Column>
          </Table.Header>
          <Table.Body>
            {packs.map((p, i) => {
              const sub = (p.sub_product_code || '').replace(/^sp_tcaca_codebuddyide_?/, '') || (p.package_code || '').replace(/^TCACA_/, '')
              return (
                <Table.Row key={i} id={i}>
                  <Table.Cell>
                    <span className="flex items-start gap-2">
                      <span className="mt-1.5 size-2.5 shrink-0 rounded-sm" style={{ background: colorOf(pkSourceKey(p)) }} />
                      <span className="flex flex-col">
                        {p.name || '(未命名)'}
                        {sub && <span className="font-mono text-xs text-muted">{sub}</span>}
                      </span>
                    </span>
                  </Table.Cell>
                  <Table.Cell className="text-right font-mono tabular-nums">{fmtTok(p.size)}</Table.Cell>
                  <Table.Cell className="text-right font-mono tabular-nums">{fmtTok(p.remain)}</Table.Cell>
                  <Table.Cell className="text-right font-mono tabular-nums">{fmtTok(p.used)}</Table.Cell>
                  <Table.Cell className="whitespace-nowrap text-right font-mono tabular-nums">{(p.created_at || '').slice(0, 16).replace('T', ' ') || '—'}</Table.Cell>
                  <Table.Cell className="whitespace-nowrap text-right font-mono tabular-nums">{(p.end_time || '').slice(0, 10) || '—'}</Table.Cell>
                </Table.Row>
              )
            })}
          </Table.Body>
        </Table.Content>
      </Table.ScrollContainer>
    </Table>
  )
}

/** 单账号逐包明细：按最早到期默认展示 N 条，其余未用完的包和已用完的包各自折叠 */
function Detail({ a, limit, colorOf }: { a: PackageAccount; limit: number; colorOf: (key: string) => string }) {
  const g = pkDetailGroups(a.packages || [], limit)
  const name = (a.nickname || a.uid.slice(0, 8)) + ' · ' + (a.realm || '')
  return (
    <Panel
      title={name}
      desc={
        '余额 ' + fmtTok(a.remain) + ' / 总额 ' + fmtTok(a.size) + ' · 可用 ' + (g.visible.length + g.rest.length) + ' 个包' +
        (g.used.length ? ' / 已用完 ' + g.used.length + ' 个' : '') + ' · 默认展示最早到期 ' + pkDetailLimitValue(limit) + ' 条'
      }
      contentClassName="flex flex-col gap-2"
    >
      {g.visible.length > 0 ? <PackTable packs={g.visible} colorOf={colorOf} label={name + ' 积分包'} /> : <Empty title="没有未用完的积分包" />}
      {(g.rest.length > 0 || g.used.length > 0) && (
        <Accordion allowsMultipleExpanded>
          {g.rest.length > 0 ? (
            <Accordion.Item id="rest">
              <Accordion.Heading>
                <Accordion.Trigger className="text-sm">
                  其余未用完 {g.rest.length} 个包（面额合计 {fmtTok(g.restSize)} · 剩余 {fmtTok(g.restRemain)}）
                  <Accordion.Indicator><ChevronDown /></Accordion.Indicator>
                </Accordion.Trigger>
              </Accordion.Heading>
              <Accordion.Panel><Accordion.Body><PackTable packs={g.rest} colorOf={colorOf} label="其余未用完的包" /></Accordion.Body></Accordion.Panel>
            </Accordion.Item>
          ) : null}
          {g.used.length > 0 ? (
            <Accordion.Item id="used">
              <Accordion.Heading>
                <Accordion.Trigger className="text-sm">
                  已用完 {g.used.length} 个包（面额合计 {fmtTok(g.usedSize)}）
                  <Accordion.Indicator><ChevronDown /></Accordion.Indicator>
                </Accordion.Trigger>
              </Accordion.Heading>
              <Accordion.Panel><Accordion.Body><PackTable packs={g.used} colorOf={colorOf} label="已用完的包" /></Accordion.Body></Accordion.Panel>
            </Accordion.Item>
          ) : null}
        </Accordion>
      )}
    </Panel>
  )
}
