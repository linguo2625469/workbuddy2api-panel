// 用量：所选时间窗口内的请求数与 token 用量（卡片、时序图、按账号 / 模型 / 域三张表同一口径）。
import { useMemo, useState, type ReactNode } from 'react'
import { Label, ListBox, Select, Table, type SortDescriptor } from '@heroui/react'
import { Bar, BarChart, CartesianGrid, ReferenceLine, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts'
import { fmtMs, fmtRate, fmtTok } from '../../lib/format'
import type { TipProps } from '../../lib/chart'
import { useUsage } from '../../lib/queries'
import type { KeyedUsage, UsagePoint } from '../../lib/types'
import { chartPoints, dayBoundaries, minGap, nearestTicks, tickLabel, type ChartPoint } from '../../lib/usage'
import { BusyButton, Empty, Loaded } from '../../components/Feedback'
import { Panel, Stat } from '../../components/Panel'

const WINDOWS = [
  { id: '24', label: '近 24 小时' },
  { id: '72', label: '近 3 天' },
  { id: '168', label: '近 7 天' },
  { id: '720', label: '近 30 天' },
  { id: '0', label: '全部历史' },
]

export function UsagePage() {
  const [hours, setHours] = useState('72')
  const query = useUsage(hours)
  const d = query.data
  const winLabel = WINDOWS.find((w) => w.id === hours)?.label ?? ''

  return (
    <>
      <Panel
        title="用量总览"
        desc={d && [
          winLabel,
          d.buckets + ' 个分桶',
          d.since && '数据自 ' + d.since.replace('T', ' '),
          d.file_bytes && '文件 ' + (d.file_bytes / 1024).toFixed(1) + ' KB',
        ].filter(Boolean).join(' · ')}
        actions={
          <>
            <Select aria-label="时间窗口" value={hours} onChange={(v) => v != null && setHours(String(v))} className="w-32">
              <Label className="sr-only">时间窗口</Label>
              <Select.Trigger><Select.Value /><Select.Indicator /></Select.Trigger>
              <Select.Popover>
                <ListBox>
                  {WINDOWS.map((w) => <ListBox.Item key={w.id} id={w.id} textValue={w.label}>{w.label}<ListBox.ItemIndicator /></ListBox.Item>)}
                </ListBox>
              </Select.Popover>
            </Select>
            <BusyButton size="sm" variant="secondary" busy={query.isFetching} onPress={() => void query.refetch()}>刷新</BusyButton>
          </>
        }
        contentClassName="flex flex-col gap-4"
      >
        <Loaded query={query} rows={4}>
          {(u) => (
            <>
              <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 xl:grid-cols-6">
                <Stat label="请求数" value={fmtTok(u.totals.requests)} />
                <Stat label="总 token" value={fmtTok(u.totals.total_tokens)} />
                <Stat label="prompt" value={fmtTok(u.totals.prompt_tokens)} />
                <Stat label="completion" value={fmtTok(u.totals.completion_tokens)} />
                <Stat label="失败尝试" value={u.totals.errors || 0} tone={u.totals.errors ? 'warning' : undefined} />
                <Stat label="平均延迟" value={fmtMs(u.totals.avg_latency_ms)} />
              </div>
              <UsageChart series={u.series || []} />
            </>
          )}
        </Loaded>
      </Panel>

      {d && (
        <>
          <Panel title="按账号" desc="请求数含失败尝试">
            <UsageTable kind="account" rows={d.by_account || []} />
          </Panel>
          <Panel title="按模型"><UsageTable kind="model" rows={d.by_model || []} /></Panel>
          <Panel title="按域"><UsageTable kind="realm" rows={d.by_realm || []} /></Panel>
        </>
      )}
    </>
  )
}

function ChartTip({ active, payload }: TipProps) {
  const p = payload?.[0]?.payload as ChartPoint | undefined
  if (!active || !p) return null
  return (
    <div className="rounded-xl border border-separator bg-overlay px-3 py-2 text-xs shadow-lg">
      <div className="mb-1 font-medium">{p.raw.replace('T', ' ') + (p.scope === 'hour' ? ':00' : '')}</div>
      <div className="flex items-center gap-2"><span className="size-2 rounded-sm bg-(--viz-1)" />prompt <b className="ml-auto pl-3 tabular-nums">{fmtTok(p.prompt)}</b></div>
      <div className="flex items-center gap-2"><span className="size-2 rounded-sm bg-(--viz-2)" />completion <b className="ml-auto pl-3 tabular-nums">{fmtTok(p.completion)}</b></div>
      <div className="mt-1 text-muted">{p.requests} 次请求</div>
    </div>
  )
}

function UsageChart({ series }: { series: UsagePoint[] }) {
  const pts = useMemo(() => chartPoints(series), [series])
  const byT = useMemo(() => new Map(pts.map((p) => [p.t, p])), [pts])
  if (!pts.length) return <Empty title="暂无用量数据">发起一次对话后再刷新。</Empty>
  const pad = minGap(pts) / 2
  const tickStyle = { fill: 'var(--muted)', fontSize: 11 }
  // 堆叠段之间描一道底色细缝；柱子多到只有一两像素宽（30 天小时桶）时不描，否则会把柱子吃掉
  const seam = pts.length <= 120 ? 1 : 0
  return (
    <div className="rounded-2xl border border-separator p-3">
      <div className="mb-2 flex flex-wrap items-center gap-3 text-xs">
        <span className="font-medium">Token 时序</span>
        <span className="ml-auto flex items-center gap-1.5 text-muted"><span className="size-2.5 rounded-sm bg-(--viz-1)" />prompt</span>
        <span className="flex items-center gap-1.5 text-muted"><span className="size-2.5 rounded-sm bg-(--viz-2)" />completion</span>
      </div>
      <ResponsiveContainer width="100%" height={220}>
        <BarChart data={pts} margin={{ top: 8, right: 8, bottom: 0, left: 0 }}>
          <CartesianGrid vertical={false} stroke="var(--separator)" strokeDasharray="2 3" />
          <XAxis
            dataKey="t" type="number" scale="time" domain={[pts[0].t - pad, pts[pts.length - 1].t + pad]}
            ticks={nearestTicks(pts)} tickFormatter={(t: number) => tickLabel(byT.get(t) ?? { t, scope: 'hour' })}
            tick={tickStyle} tickLine={false} axisLine={{ stroke: 'var(--border)' }}
          />
          <YAxis tickFormatter={(v: number) => fmtTok(v)} width={48} tick={tickStyle} tickLine={false} axisLine={false} />
          <Tooltip content={ChartTip} cursor={{ fill: 'var(--surface-secondary)' }} />
          {dayBoundaries(pts).map((x) => <ReferenceLine key={x} x={x - pad} stroke="var(--separator)" />)}
          <Bar dataKey="prompt" stackId="tok" fill="var(--viz-1)" stroke="var(--surface)" strokeWidth={seam} maxBarSize={30} isAnimationActive={false} />
          <Bar dataKey="completion" stackId="tok" fill="var(--viz-2)" stroke="var(--surface)" strokeWidth={seam} maxBarSize={30} radius={[4, 4, 0, 0]} isAnimationActive={false} />
        </BarChart>
      </ResponsiveContainer>
    </div>
  )
}

type UsageKind = 'account' | 'model' | 'realm'
type NumKey = 'requests' | 'errors' | 'prompt_tokens' | 'completion_tokens' | 'total_tokens' | 'avg_latency_ms' | 'avg_tokens_per_second'

const NUM_COLS: { id: NumKey; label: string; fmt: (r: KeyedUsage) => ReactNode; perf?: boolean }[] = [
  { id: 'requests', label: '请求', fmt: (r) => fmtTok(r.requests) },
  { id: 'errors', label: '失败', fmt: (r) => (r.errors ? <span className="text-warning">{fmtTok(r.errors)}</span> : '—') },
  { id: 'prompt_tokens', label: 'Prompt', fmt: (r) => fmtTok(r.prompt_tokens) },
  { id: 'completion_tokens', label: 'Completion', fmt: (r) => fmtTok(r.completion_tokens) },
  { id: 'total_tokens', label: '合计', fmt: (r) => fmtTok(r.total_tokens) },
  { id: 'avg_latency_ms', label: '均延迟', fmt: (r) => fmtMs(r.avg_latency_ms), perf: true },
  { id: 'avg_tokens_per_second', label: '均速率', fmt: (r) => fmtRate(r.avg_tokens_per_second), perf: true },
]

/** 按账号 / 模型 / 域的用量表；数字列点表头排序。账号行显示昵称（后端放在 extra 里），UID 做副标题 */
function UsageTable({ kind, rows }: { kind: UsageKind; rows: KeyedUsage[] }) {
  const [sort, setSort] = useState<SortDescriptor | undefined>()
  const cols = NUM_COLS.filter((c) => !c.perf || kind === 'account')
  const sorted = useMemo(() => {
    if (!sort) return rows
    const k = sort.column as NumKey
    const out = [...rows].sort((a, b) => (a[k] || 0) - (b[k] || 0))
    return sort.direction === 'descending' ? out.reverse() : out
  }, [rows, sort])
  if (!rows.length) return <Empty title="暂无数据" />
  return (
    <Table variant="secondary">
      <Table.ScrollContainer>
        <Table.Content aria-label={kind === 'account' ? '按账号' : kind === 'model' ? '按模型' : '按域'} className="min-w-[640px]" sortDescriptor={sort} onSortChange={setSort}>
          <Table.Header>
            <Table.Column id="key" isRowHeader>{kind === 'account' ? '账号' : kind === 'model' ? '模型' : 'realm'}</Table.Column>
            {kind === 'account' ? <Table.Column id="realm">域</Table.Column> : null}
            {cols.map((c) => (
              <Table.Column key={c.id} id={c.id} allowsSorting className="text-right">
                {({ sortDirection }) => <Table.SortableColumnHeader sortDirection={sortDirection} className="justify-end gap-1">{c.label}</Table.SortableColumnHeader>}
              </Table.Column>
            ))}
          </Table.Header>
          <Table.Body>
            {sorted.map((r) => (
              <Table.Row key={r.key} id={r.key}>
                <Table.Cell>
                  {kind === 'account' ? (
                    <div className="flex flex-col">
                      <span className="font-medium">{r.extra || r.key.slice(0, 8)}</span>
                      {r.extra && <span className="font-mono text-xs text-muted">{r.key.slice(0, 8)}</span>}
                    </div>
                  ) : <span className="break-all">{r.key}</span>}
                </Table.Cell>
                {kind === 'account' ? <Table.Cell className="text-muted">{r.realm || '—'}</Table.Cell> : null}
                {cols.map((c) => <Table.Cell key={c.id} className="text-right font-mono tabular-nums">{c.fmt(r)}</Table.Cell>)}
              </Table.Row>
            ))}
          </Table.Body>
        </Table.Content>
      </Table.ScrollContainer>
    </Table>
  )
}
