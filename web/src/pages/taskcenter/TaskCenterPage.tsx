// 任务中心：扫描全部账号的成长任务待办，排成一列一键执行（账号间可并发，账号内串行）。
// 执行时轮询队列状态；切走再回来，若本页会话启动（或正在执行）的队列还在跑，会接着显示进度。
import { useEffect, useRef, useState } from 'react'
import { Button, Chip, Label, ListBox, ProgressBar, Select, toast } from '@heroui/react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { post } from '../../lib/api'
import { timeMs } from '../../lib/format'
import { toastError } from '../../lib/hooks'
import { qk, useQueue } from '../../lib/queries'
import { growthTitles } from '../../lib/tasks'
import type { QueueItem, QueueState, RunQueueResp, ScanAccount } from '../../lib/types'
import { BusyButton, Empty } from '../../components/Feedback'
import { Panel } from '../../components/Panel'
import { useConfirm } from '../../components/useConfirm'
import { VouchersModal } from './VouchersModal'

interface Row { kind: string; code: string; prog: string; status: string; message?: string }
interface Group { uid: string; nick: string; rows: Row[] }

// 本页会话里跟踪过的队列代次：只显示自己启动的、或打开页面时仍在执行的那一轮；
// 页面打开前就已结束的旧队列不接管（否则会冲掉用户刚扫描的结果）。
let trackedSeq = 0

function groupsFromScan(accounts: ScanAccount[]): Group[] {
  const out: Group[] = []
  for (const a of accounts) {
    const rows: Row[] = []
    for (const t of a.growth || []) {
      growthTitles.set(t.task_code, t.title || t.task_code)
      rows.push({ kind: 'growth', code: t.task_code, prog: t.target ? t.current + '/' + t.target : '—', status: 'scan' })
    }
    if (rows.length) out.push({ uid: a.uid, nick: a.nickname, rows })
  }
  return out
}

function groupsFromQueue(items: QueueItem[]): Group[] {
  const by = new Map<string, Group>()
  for (const it of items) {
    if (!by.has(it.uid)) by.set(it.uid, { uid: it.uid, nick: it.nickname, rows: [] })
    by.get(it.uid)!.rows.push({ kind: it.kind, code: it.code, prog: it.kind === 'school' ? '—' : '', status: it.status, message: it.message })
  }
  return [...by.values()]
}

const STATUS: Record<string, { label: string; color: 'success' | 'accent' | 'danger' | 'default' | 'warning' }> = {
  done: { label: '完成', color: 'success' },
  running: { label: '执行中', color: 'accent' },
  error: { label: '失败', color: 'danger' },
  skipped: { label: '跳过', color: 'default' },
  pending: { label: '排队', color: 'default' },
  scan: { label: '待执行', color: 'warning' },
}

export function TaskCenterPage() {
  const qc = useQueryClient()
  const queue = useQueue()
  // 扫描结果放在查询缓存里：切到别的页面再回来，上次的扫描结果还在
  const scan = useQuery({
    queryKey: ['tasks-scan'],
    queryFn: () => post<{ accounts: ScanAccount[] | null }>('tasks/scan_all'),
    enabled: false,
    staleTime: Infinity,
    gcTime: Infinity,
  })
  const [conc, setConc] = useState('1')
  const [starting, setStarting] = useState(false)
  const [vouchers, setVouchers] = useState(false)
  const [confirmEl, ask] = useConfirm()

  const q = queue.data
  // 打开页面时队列正在执行：接管这一轮
  const tracked = trackedSeq || (q?.running ? q.seq : 0)
  const queueStarted = timeMs(q?.started_at) ?? 0
  const showQueue = !!q?.started && q.seq === tracked && (q.running || queueStarted >= scan.dataUpdatedAt)

  // 本页跟踪的队列从执行中变为结束时提示一次
  const wasRunning = useRef(false)
  useEffect(() => {
    if (!q) return
    if (q.running && trackedSeq === 0) trackedSeq = q.seq
    if (q.seq !== trackedSeq) return
    if (wasRunning.current && !q.running) toast.success('任务队列执行结束')
    wasRunning.current = q.running
  }, [q])

  const doScan = async () => {
    const r = await scan.refetch()
    if (r.error) toastError(r.error)
  }

  const runQueue = async () => {
    setStarting(true)
    try {
      const r = await post<RunQueueResp>('tasks/run_queue', { concurrency: Number(conc) || 1 })
      if (!r.started) {
        toast.success(r.message || '没有待办任务')
        return
      }
      trackedSeq = r.seq || 0
      toast.success('队列已启动：' + r.total + ' 项（并发 ' + conc + '）')
      await qc.invalidateQueries({ queryKey: qk.queue })
    } catch (e) {
      toastError(e)
    } finally {
      setStarting(false)
    }
  }

  const groups = showQueue ? groupsFromQueue(q?.items || []) : scan.data ? groupsFromScan(scan.data.accounts || []) : null
  const total = groups?.reduce((n, g) => n + g.rows.length, 0) ?? 0

  return (
    <>
      <Panel
        title={<>成长任务队列{total > 0 && <span className="ml-2 text-sm font-normal text-muted">{total} 项</span>}</>}
        actions={
          <>
            <Button size="sm" variant="ghost" onPress={() => setVouchers(true)}>查询券码</Button>
            <Select aria-label="并发" value={conc} onChange={(v) => v != null && setConc(String(v))} className="w-28">
              <Label className="sr-only">并发</Label>
              <Select.Trigger>
                <Select.Value>{({ defaultChildren }) => <>并发 {defaultChildren}</>}</Select.Value>
                <Select.Indicator />
              </Select.Trigger>
              <Select.Popover>
                <ListBox>
                  {['1', '2', '3'].map((n) => (
                    <ListBox.Item key={n} id={n} textValue={n}>{n}<ListBox.ItemIndicator /></ListBox.Item>
                  ))}
                </ListBox>
              </Select.Popover>
            </Select>
            <BusyButton size="sm" variant="secondary" busy={scan.isFetching} onPress={() => void doScan()}>
              {scan.isFetching ? '扫描中…' : '扫描待办'}
            </BusyButton>
            <BusyButton
              size="sm"
              variant="primary"
              busy={starting}
              isDisabled={!!q?.running}
              onPress={() => ask({
                title: '执行全部待办？',
                body: `扫描全部账号待办并排队执行（账号并发 ${conc}，账号内串行）。含真实对话的任务耗时较长。`,
                confirmLabel: '开始执行',
                danger: false,
                onConfirm: () => void runQueue(),
              })}
            >
              {q?.running ? '执行中…' : '执行全部待办'}
            </BusyButton>
          </>
        }
        contentClassName="flex flex-col gap-4"
      >
        {showQueue && q && <QueueProgress q={q} />}
        {groups === null ? (
          <Empty title="还没有扫描过">扫描所有账号的成长任务待办，把没做的排成一列，一键执行。</Empty>
        ) : groups.length === 0 ? (
          <Empty title="没有待办任务 🎉">全部账号的成长任务都已完成，明日再来。</Empty>
        ) : (
          groups.map((g) => (
            <section key={g.uid} className="flex flex-col">
              <h3 className="flex items-baseline gap-2 pb-1 text-sm">
                <span className="font-semibold">{g.nick || g.uid.slice(0, 12)}</span>
                <span className="text-xs text-muted">{g.rows.length} 项</span>
              </h3>
              <ul className="flex flex-col divide-y divide-separator">
                {g.rows.map((r, i) => {
                  const st = STATUS[r.status] ?? { label: r.status, color: 'default' as const }
                  return (
                    <li key={r.code + i} className="grid grid-cols-[1fr_auto] items-center gap-x-3 gap-y-0.5 py-2 text-sm sm:grid-cols-[10rem_1fr_4rem_6rem_minmax(8rem,16rem)]">
                      <span className="hidden truncate font-mono text-xs text-muted sm:block">{r.code}</span>
                      <span className="flex min-w-0 items-center gap-1.5">
                        <span className="truncate">{r.kind === 'school' ? '开学季闭环' : growthTitles.get(r.code) || r.code}</span>
                        {r.kind === 'school' && <Chip size="sm" variant="soft">开学季</Chip>}
                      </span>
                      <span className="hidden text-right tabular-nums text-muted sm:block">{r.prog}</span>
                      <span className="justify-self-end sm:justify-self-start"><Chip size="sm" variant="soft" color={st.color}>{st.label}</Chip></span>
                      {r.message && <span className="col-span-2 break-all text-xs text-muted sm:col-span-1 sm:truncate" title={r.message}>{r.message}</span>}
                    </li>
                  )
                })}
              </ul>
            </section>
          ))
        )}
      </Panel>
      <VouchersModal open={vouchers} onClose={() => setVouchers(false)} />
      {confirmEl}
    </>
  )
}

function QueueProgress({ q }: { q: QueueState }) {
  const items = q.items || []
  const done = items.filter((it) => it.status === 'done' || it.status === 'error' || it.status === 'skipped').length
  return (
    <ProgressBar aria-label="队列进度" value={done} maxValue={Math.max(1, items.length)} color={q.running ? 'accent' : 'success'}>
      <div className="flex w-full justify-between text-sm">
        <Label>{q.running ? '执行中' : '已结束'}</Label>
        <span className="tabular-nums text-muted">{done} / {items.length}</span>
      </div>
      <ProgressBar.Track><ProgressBar.Fill /></ProgressBar.Track>
    </ProgressBar>
  )
}
