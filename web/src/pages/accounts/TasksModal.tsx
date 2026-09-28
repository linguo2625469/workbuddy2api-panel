// 单个账号的积分任务：查询上游任务进度；「接受」为报名（幂等），「领取」在进度达标后可用，
// 可自动完成的任务给「一键完成」。所有操作走网关，无需外部脚本。
import { useState } from 'react'
import { Alert, Button, Chip, CloseButton, Modal, Table, toast, useMediaQuery } from '@heroui/react'
import { useQueryClient } from '@tanstack/react-query'
import { acct, post } from '../../lib/api'
import { toastError } from '../../lib/hooks'
import { qk, useTasks } from '../../lib/queries'
import { AUTO_TASKS } from '../../lib/tasks'
import type { AcceptAllResp, AutoAllItem, AutoResult, Task } from '../../lib/types'
import { BusyButton, Empty, Loaded } from '../../components/Feedback'
import { useConfirm } from '../../components/useConfirm'

export function TasksModal({ uid, name, onClose }: { uid: string | null; name: string; onClose: () => void }) {
  const isDesktop = useMediaQuery('(min-width: 768px)')
  return (
    <Modal.Backdrop isOpen={!!uid} onOpenChange={(v) => { if (!v) onClose() }}>
      <Modal.Container size={isDesktop ? 'lg' : 'full'} scroll="inside">
        <Modal.Dialog className={isDesktop ? 'sm:max-w-5xl' : undefined}>
          <Modal.CloseTrigger />
          <Modal.Header>
            <Modal.Heading>积分任务 <span className="text-sm font-normal text-muted">{name}</span></Modal.Heading>
            <p className="mt-1.5 text-sm text-muted">「接受」为报名（幂等），「领取」在进度达标后可用。所有操作走网关，无需外部脚本。</p>
          </Modal.Header>
          {uid && <TasksBody key={uid} uid={uid} wide={isDesktop} onClose={onClose} />}
        </Modal.Dialog>
      </Modal.Container>
    </Modal.Backdrop>
  )
}

function badge(t: Task) {
  if (t.claimed) return <Chip size="sm" variant="soft" color="success">已领取</Chip>
  if (t.claimable) return <Chip size="sm" variant="soft" color="warning">可领取</Chip>
  if (t.locked) return <Chip size="sm" variant="soft">未解锁</Chip>
  if (t.accept_status === 'accepted') return <Chip size="sm" variant="soft">进行中</Chip>
  return <Chip size="sm" variant="soft">未接受</Chip>
}

const progress = (t: Task) => {
  const cur = t.current ?? 0, tgt = t.target ?? 0
  return tgt ? cur + ' / ' + tgt : cur > 0 ? String(cur) : '—'
}

const reward = (t: Task) => {
  const parts = []
  if (t.credit) parts.push('+' + t.credit + ' 分')
  if (t.energy) parts.push('+' + t.energy + ' 能')
  if (t.reward_buddy) parts.push('Buddy')
  return parts.join(' ') || '—'
}

function TasksBody({ uid, wide, onClose }: { uid: string; wide: boolean; onClose: () => void }) {
  const qc = useQueryClient()
  const query = useTasks(uid)
  const [busy, setBusy] = useState<string | null>(null)
  const [autoAll, setAutoAll] = useState<AutoAllItem[] | null>(null)
  const [confirmEl, ask] = useConfirm()
  const reload = () => void qc.invalidateQueries({ queryKey: qk.tasks(uid) })

  const act = async (kind: 'accept' | 'claim' | 'auto', code: string) => {
    setBusy(kind + ':' + code)
    try {
      if (kind === 'auto') {
        // 一键完成：后端执行动作 → 回读进度 → 汇报（耗时可到分钟级，含真实对话）
        const r = await post<AutoResult>(acct(uid, 'tasks/auto'), { task_code: code })
        if (r.skipped) {
          toast.success(r.message || '已跳过')
        } else {
          const advanced = r.progress_before !== r.progress_after
          let msg = r.message || '已执行'
          if (r.progress_after) msg += `（进度 ${r.progress_before} → ${r.progress_after}）`
          if (r.claimed) msg += '，奖励已自动到账'
          else if (r.claimable) msg += r.claim_error ? '，可点「领取」重试' : ''
          else if (r.attempt && !advanced) msg += '；进度未动，该任务可能需要官方客户端'
          if (r.claimed || advanced) toast.success(msg)
          else toast.danger(msg)
        }
        void qc.invalidateQueries({ queryKey: qk.overview })
      } else if (kind === 'claim') {
        await post(acct(uid, 'tasks/claim'), { task_code: code })
        toast.success('已领取奖励')
        void qc.invalidateQueries({ queryKey: qk.overview })
      } else {
        await post(acct(uid, 'tasks/accept'), { task_codes: [code] })
        toast.success('已接受任务')
      }
    } catch (e) {
      toastError(e)
    } finally {
      setBusy(null)
      reload()
    }
  }

  const acceptAll = async () => {
    setBusy('accept_all')
    try {
      const r = await post<AcceptAllResp>(acct(uid, 'tasks/accept_all'))
      const n = r.accepted || 0
      if (r.failed?.length) toast.danger(`已接受 ${n} 个，${r.failed.length} 个被上游拒绝（可重试）`, { description: r.failed.join('、') })
      else toast.success(n ? `已接受 ${n} 个任务` : r.message || '所有任务均已接受')
    } catch (e) {
      toastError(e)
    } finally {
      setBusy(null)
      reload()
    }
  }

  const runAutoAll = async () => {
    setBusy('auto_all')
    setAutoAll(null)
    try {
      const r = await post<{ results: AutoAllItem[] | null }>(acct(uid, 'tasks/auto_all'))
      setAutoAll(r.results || [])
      void qc.invalidateQueries({ queryKey: qk.overview })
    } catch (e) {
      toastError(e)
    } finally {
      setBusy(null)
      reload()
    }
  }

  const actionFor = (t: Task) => {
    if (t.claimed || t.locked) return null
    const b = (kind: 'accept' | 'claim' | 'auto', label: string, variant: 'primary' | 'secondary') => (
      <BusyButton size="sm" variant={variant} busy={busy === kind + ':' + t.task_code} isDisabled={!!busy && busy !== kind + ':' + t.task_code}
        onPress={() => void act(kind, t.task_code)}>{label}</BusyButton>
    )
    if (t.claimable) return b('claim', '领取', 'primary')
    if (AUTO_TASKS[t.task_code]) return b('auto', '一键完成', 'primary')
    if (t.accept_status === 'accepted') return null
    return b('accept', '接受', 'secondary')
  }

  return (
    <>
      <Modal.Body className="flex flex-col gap-4">
        {autoAll && <AutoAllResult items={autoAll} onDismiss={() => setAutoAll(null)} />}
        <Loaded query={query} rows={6}>
          {(d) => {
            // 有进度或可领取的排前面，已领取沉底——一眼看到「现在该做什么」
            const list = [...(d.tasks || [])].sort((a, b) =>
              Number(!!a.claimed) - Number(!!b.claimed) || Number(!!b.claimable) - Number(!!a.claimable) ||
              String(a.task_code).localeCompare(String(b.task_code)))
            if (!list.length) return <Empty title="该账号暂无任务" />
            const guide = (t: Task) => [t.task_desc || t.description, AUTO_TASKS[t.task_code] && '可自动：' + AUTO_TASKS[t.task_code], t.jump_url && '跳转：' + t.jump_url].filter(Boolean).join('\n')
            if (!wide) {
              return (
                <ul className="flex flex-col divide-y divide-separator">
                  {list.map((t) => (
                    <li key={t.task_code} className="flex flex-col gap-1.5 py-3">
                      <div className="flex items-start justify-between gap-2">
                        <span className="font-medium">{t.title || t.task_code}</span>
                        {badge(t)}
                      </div>
                      <span className="font-mono text-xs text-muted">{t.task_code}{t.tag ? ' · ' + t.tag : ''}</span>
                      {guide(t) && <p className="whitespace-pre-line text-xs text-muted">{guide(t)}</p>}
                      <div className="flex items-center justify-between gap-2 text-sm">
                        <span className="tabular-nums text-muted">进度 {progress(t)} · {reward(t)}</span>
                        {actionFor(t)}
                      </div>
                    </li>
                  ))}
                </ul>
              )
            }
            return (
              <Table variant="secondary">
                <Table.ScrollContainer>
                  <Table.Content aria-label="积分任务">
                    <Table.Header>
                      <Table.Column isRowHeader>任务</Table.Column>
                      <Table.Column>进度</Table.Column>
                      <Table.Column>奖励</Table.Column>
                      <Table.Column>状态</Table.Column>
                      <Table.Column className="w-0"> </Table.Column>
                    </Table.Header>
                    <Table.Body>
                      {list.map((t) => (
                        <Table.Row key={t.task_code} id={t.task_code}>
                          <Table.Cell>
                            <div className="flex max-w-md flex-col gap-0.5">
                              <span className="font-medium">{t.title || t.task_code}</span>
                              <span className="font-mono text-xs text-muted">{t.task_code}{t.tag ? ' · ' + t.tag : ''}</span>
                              {guide(t) && <span className="line-clamp-3 whitespace-pre-line text-xs text-muted" title={guide(t)}>{guide(t)}</span>}
                            </div>
                          </Table.Cell>
                          <Table.Cell className="whitespace-nowrap tabular-nums">{progress(t)}</Table.Cell>
                          <Table.Cell className="whitespace-nowrap">{reward(t)}</Table.Cell>
                          <Table.Cell>{badge(t)}</Table.Cell>
                          <Table.Cell>{actionFor(t)}</Table.Cell>
                        </Table.Row>
                      ))}
                    </Table.Body>
                  </Table.Content>
                </Table.ScrollContainer>
              </Table>
            )
          }}
        </Loaded>
      </Modal.Body>
      <Modal.Footer className="flex-wrap">
        <Button variant="tertiary" onPress={onClose}>关闭</Button>
        <Button variant="secondary" isDisabled={!!busy} onPress={reload}>重新查询</Button>
        <BusyButton variant="secondary" busy={busy === 'accept_all'} isDisabled={!!busy && busy !== 'accept_all'} onPress={() => void acceptAll()}>全部接受</BusyButton>
        <BusyButton
          variant="primary"
          busy={busy === 'auto_all'}
          isDisabled={!!busy && busy !== 'auto_all'}
          onPress={() => ask({
            title: '一键完成可自动任务？',
            body: '将依次执行：补报对话事件、领取 Buddy、glm-5.2 对话、尝试上报。过程约 1–2 分钟（含真实对话）。',
            confirmLabel: '开始执行',
            danger: false,
            onConfirm: () => void runAutoAll(),
          })}
        >
          {busy === 'auto_all' ? '执行中…' : '一键完成可自动任务'}
        </BusyButton>
      </Modal.Footer>
      {confirmEl}
    </>
  )
}

const STATUS: Record<string, { label: string; color: 'success' | 'danger' | 'default' }> = {
  done: { label: '完成', color: 'success' },
  error: { label: '失败', color: 'danger' },
  skipped: { label: '跳过', color: 'default' },
}

/** 「一键完成可自动任务」的逐项结果（原先只打印在浏览器控制台） */
function AutoAllResult({ items, onDismiss }: { items: AutoAllItem[]; onDismiss: () => void }) {
  const n = (s: string) => items.filter((x) => x.status === s).length
  const err = n('error')
  return (
    <Alert status={err ? 'warning' : 'success'}>
      <Alert.Indicator />
      <Alert.Content className="min-w-0">
        <Alert.Title>执行完成：成功 {n('done')} 项，跳过 {n('skipped')} 项{err ? '，失败 ' + err + ' 项' : ''}</Alert.Title>
        <Alert.Description>
          <ul className="mt-2 flex max-h-64 flex-col gap-1.5 overflow-y-auto">
            {items.map((it, i) => {
              const st = STATUS[it.status] ?? { label: it.status, color: 'default' as const }
              return (
                <li key={i} className="flex items-start gap-2 text-xs">
                  <Chip size="sm" variant="soft" color={st.color} className="shrink-0">{st.label}</Chip>
                  <span className="min-w-0">
                    <span className="font-mono">{it.task_code}</span>
                    {it.message && <span className="text-muted"> — {it.message}</span>}
                  </span>
                </li>
              )
            })}
          </ul>
        </Alert.Description>
      </Alert.Content>
      <CloseButton aria-label="收起结果" onPress={onDismiss} />
    </Alert>
  )
}
