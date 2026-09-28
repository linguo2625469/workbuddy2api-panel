// 账号池：统计（点一下按状态筛选）+ 搜索 + 账号表（可排序，点行看详情）+ 批量任务。窄屏把表格换成卡片列表。
import { useMemo, useState } from 'react'
import {
  Button, Card, SearchField, Table, ToggleButton, ToggleButtonGroup, useMediaQuery, type SortDescriptor,
} from '@heroui/react'
import { accountHealth, filterAccounts, sortAccounts, type AccountSortKey, type StatusFilter } from '../../lib/accounts'
import { post } from '../../lib/api'
import { ago } from '../../lib/format'
import { toastError, useNow, useStartedToast } from '../../lib/hooks'
import { useOverview } from '../../lib/queries'
import type { Account, Overview } from '../../lib/types'
import { Empty, Loaded } from '../../components/Feedback'
import { Panel, Stat } from '../../components/Panel'
import { AccountButtons } from './AccountButtons'
import { AccountName, CreditsCell, StatusCell, UsageChips } from './AccountCells'
import { AccountDrawer } from './AccountDrawer'
import { TasksModal } from './TasksModal'
import { useAccountActions } from './useAccountActions'

export function AccountsPage() {
  const query = useOverview(5000)
  return (
    <Loaded query={query} rows={6}>
      {(d) => <Accounts data={d} fetchedAt={query.dataUpdatedAt} />}
    </Loaded>
  )
}

const BATCH: { path: string; label: string; done: string }[] = [
  { path: 'checkin_all', label: '全部签到', done: '全部签到已开始' },
  { path: 'travel_all', label: '旅行巡检', done: '旅行巡检已开始（含领养链路）' },
  { path: 'activity_all', label: '活跃上报', done: '活跃上报已开始' },
  { path: 'keepalive_all', label: '全部保活', done: '全部保活已开始' },
]

function Accounts({ data, fetchedAt }: { data: Overview; fetchedAt: number }) {
  const now = useNow()
  const isDesktop = useMediaQuery('(min-width: 1024px)')
  const [status, setStatus] = useState<StatusFilter>('all')
  const [q, setQ] = useState('')
  const [sort, setSort] = useState<SortDescriptor | undefined>()
  const [detail, setDetail] = useState<string | null>(null)
  const [tasksOf, setTasksOf] = useState<string | null>(null)
  const { busy, run, confirmEl } = useAccountActions(setTasksOf)

  const accounts = useMemo(() => data.accounts || [], [data])
  const poolMax = Math.max(1, ...accounts.map((a) => a.credits || 0))
  const rows = useMemo(
    () => sortAccounts(filterAccounts(accounts, fetchedAt, status, q), (sort?.column as AccountSortKey) ?? null, sort?.direction === 'descending'),
    [accounts, fetchedAt, status, q, sort],
  )
  const remSum = accounts.reduce((s, a) => s + (a.credits || 0), 0)
  const totSum = accounts.reduce((s, a) => s + (a.credits_total || 0), 0)
  const byUid = (uid: string | null) => accounts.find((a) => a.uid === uid) ?? null
  const detailAcct = byUid(detail)
  const tasksAcct = byUid(tasksOf)

  const tiles: { id: StatusFilter; label: string; value: number; color?: string }[] = [
    { id: 'all', label: '账号总数', value: data.total },
    { id: 'ok', label: '可用', value: data.healthy, color: 'text-success' },
    { id: 'cooling', label: '冷却中', value: data.cooling, color: 'text-warning' },
    { id: 'disabled', label: '已禁用', value: data.disabled, color: 'text-danger' },
  ]

  return (
    <>
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 xl:grid-cols-6">
        <ToggleButtonGroup
          aria-label="按状态筛选"
          isDetached
          selectionMode="single"
          disallowEmptySelection
          selectedKeys={[status]}
          onSelectionChange={(keys) => setStatus(([...keys][0] as StatusFilter) ?? 'all')}
          className="contents"
        >
          {tiles.map((t) => (
            <ToggleButton key={t.id} id={t.id} className="h-auto w-full flex-col items-start justify-start gap-1 rounded-3xl p-4 text-left">
              <span className={`text-2xl font-semibold tabular-nums ${t.color ?? ''}`}>{t.value}</span>
              <span className="text-xs font-normal text-muted">{t.label}</span>
            </ToggleButton>
          ))}
        </ToggleButtonGroup>
        <Stat label="积分剩余 / 总额" value={totSum > 0 ? `${remSum} / ${totSum}` : remSum} />
        <Stat label="粘性会话" value={data.sticky_sessions} />
      </div>

      <Panel
        title="账号池"
        desc={data.in_flight_full ? data.in_flight_full + ' 个账号在途占满' : undefined}
        actions={<BatchButtons />}
        contentClassName="flex flex-col gap-3"
      >
        <SearchField aria-label="搜索账号" value={q} onChange={setQ} className="w-full sm:w-72">
          <SearchField.Group>
            <SearchField.SearchIcon />
            <SearchField.Input placeholder="搜索昵称或 UID" />
            <SearchField.ClearButton />
          </SearchField.Group>
        </SearchField>
        {accounts.length === 0 ? (
          <Empty title="账号池是空的">点击右上角「添加账号」，用浏览器登录一个 WorkBuddy 账号。</Empty>
        ) : rows.length === 0 ? (
          <Empty title="没有符合条件的账号" action={<Button size="sm" variant="secondary" onPress={() => { setQ(''); setStatus('all') }}>清除筛选</Button>} />
        ) : isDesktop ? (
          <AccountTable rows={rows} fetchedAt={fetchedAt} now={now} poolMax={poolMax} sort={sort} onSort={setSort} busy={busy} run={run} onOpen={setDetail} />
        ) : (
          <div className="flex flex-col gap-3">
            {rows.map((a) => {
              const health = accountHealth(a, fetchedAt)
              return (
                <Card key={a.uid} variant="secondary" className="gap-3 p-4">
                  <div className="flex items-start justify-between gap-2">
                    <AccountName a={a} />
                    <Button size="sm" variant="tertiary" onPress={() => setDetail(a.uid)}>详情</Button>
                  </div>
                  <StatusCell a={a} health={health} now={now} />
                  <div className="grid grid-cols-2 gap-3 text-sm">
                    <CreditsCell a={a} poolMax={poolMax} now={now} />
                    <div className="flex flex-col gap-0.5 text-xs text-muted">
                      <span>成功 {a.success_count || 0} · 失败 <span className="text-danger">{a.err_total || 0}</span></span>
                      <span>在途 {a.in_flight || 0}</span>
                      <span>最近成功 {ago(a.last_success, now)}</span>
                    </div>
                  </div>
                  <UsageChips a={a} />
                  <AccountButtons a={a} frozen={health.state !== 'ok'} busy={busy} run={run} />
                </Card>
              )
            })}
          </div>
        )}
      </Panel>

      <AccountDrawer
        account={detailAcct}
        health={detailAcct ? accountHealth(detailAcct, fetchedAt) : null}
        now={now}
        busy={busy}
        run={run}
        onClose={() => setDetail(null)}
      />
      <TasksModal uid={tasksOf} name={tasksAcct?.nickname || tasksOf?.slice(0, 16) || ''} onClose={() => setTasksOf(null)} />
      {confirmEl}
    </>
  )
}

function AccountTable({ rows, fetchedAt, now, poolMax, sort, onSort, busy, run, onOpen }: {
  rows: Account[]
  fetchedAt: number
  now: number
  poolMax: number
  sort: SortDescriptor | undefined
  onSort: (s: SortDescriptor) => void
  busy: string | null
  run: ReturnType<typeof useAccountActions>['run']
  onOpen: (uid: string) => void
}) {
  const sortable = (id: AccountSortKey, label: string) => (
    <Table.Column id={id} allowsSorting className="whitespace-nowrap">
      {({ sortDirection }) => <Table.SortableColumnHeader sortDirection={sortDirection}>{label}</Table.SortableColumnHeader>}
    </Table.Column>
  )
  return (
    <Table variant="secondary">
      <Table.ScrollContainer>
        <Table.Content
          aria-label="账号池"
          className="min-w-[1080px]"
          sortDescriptor={sort}
          onSortChange={onSort}
          onRowAction={(key) => onOpen(String(key))}
        >
          <Table.Header>
            <Table.Column id="name" isRowHeader allowsSorting className="whitespace-nowrap">
              {({ sortDirection }) => <Table.SortableColumnHeader sortDirection={sortDirection}>账号</Table.SortableColumnHeader>}
            </Table.Column>
            <Table.Column id="status" className="whitespace-nowrap">状态</Table.Column>
            {sortable('credits', '积分')}
            {sortable('errors', '成功 / 失败')}
            <Table.Column id="usage" className="whitespace-nowrap">用量</Table.Column>
            {sortable('last_success', '最近成功')}
            <Table.Column id="actions"> </Table.Column>
          </Table.Header>
          <Table.Body>
            {rows.map((a) => {
              const health = accountHealth(a, fetchedAt)
              return (
                <Table.Row key={a.uid} id={a.uid} className="cursor-pointer">
                  <Table.Cell><div className="max-w-40"><AccountName a={a} /></div></Table.Cell>
                  <Table.Cell><StatusCell a={a} health={health} now={now} /></Table.Cell>
                  <Table.Cell><CreditsCell a={a} poolMax={poolMax} now={now} /></Table.Cell>
                  <Table.Cell className="whitespace-nowrap">
                    <div className="font-mono tabular-nums">
                      {a.success_count || 0} <span className="text-muted">/</span> <span className="text-danger">{a.err_total || 0}</span>
                    </div>
                    <div className="text-xs text-muted">在途 {a.in_flight || 0}</div>
                  </Table.Cell>
                  <Table.Cell><div className="w-36"><UsageChips a={a} /></div></Table.Cell>
                  <Table.Cell className="whitespace-nowrap text-muted">{ago(a.last_success, now)}</Table.Cell>
                  <Table.Cell><div className="flex justify-end"><AccountButtons a={a} frozen={health.state !== 'ok'} busy={busy} run={run} nowrap /></div></Table.Cell>
                </Table.Row>
              )
            })}
          </Table.Body>
        </Table.Content>
      </Table.ScrollContainer>
    </Table>
  )
}

function BatchButtons() {
  const started = useStartedToast()
  const [busy, setBusy] = useState<string | null>(null)
  const fire = async (b: (typeof BATCH)[number]) => {
    setBusy(b.path)
    try {
      await post(b.path)
      started(b.done)
    } catch (e) {
      toastError(e)
    } finally {
      setBusy(null)
    }
  }
  return BATCH.map((b) => (
    <Button key={b.path} size="sm" variant="secondary" isPending={busy === b.path} onPress={() => void fire(b)}>{b.label}</Button>
  ))
}
