// 单个账号的运维动作（签到 / 刷新余额 / 查看任务 / 解冻 / 禁用 / 移除），账号表、手机卡片、详情抽屉共用。
import { useState } from 'react'
import { toast } from '@heroui/react'
import { useQueryClient } from '@tanstack/react-query'
import { acct, post } from '../../lib/api'
import { toastError } from '../../lib/hooks'
import { qk } from '../../lib/queries'
import type { Account } from '../../lib/types'
import { useConfirm } from '../../components/useConfirm'

export type AccountAction = 'checkin' | 'balance' | 'tasks' | 'revive' | 'disable' | 'remove'

interface CheckinResp { credits?: number; credits_total?: number; checkin_message?: string; balance_error?: string }

export function useAccountActions(openTasks: (uid: string) => void) {
  const qc = useQueryClient()
  const [busy, setBusy] = useState<string | null>(null)
  const [confirmEl, ask] = useConfirm()

  const exec = async (uid: string, action: Exclude<AccountAction, 'tasks'>) => {
    setBusy(uid + ':' + action)
    try {
      if (action === 'checkin') {
        const r = await post<CheckinResp>(acct(uid, 'checkin'))
        const credits = r.credits != null ? '，积分 ' + r.credits + (r.credits_total ? '/' + r.credits_total : '') : ''
        toast.success('签到完成' + credits + (r.checkin_message ? '（' + r.checkin_message + '）' : ''))
      } else if (action === 'balance') {
        const r = await post<CheckinResp>(acct(uid, 'balance'))
        toast.success('余额已更新：' + r.credits + (r.credits_total ? ' / ' + r.credits_total : ''))
      } else if (action === 'revive') {
        await post(acct(uid, 'revive'))
        toast.success('已解冻')
      } else if (action === 'disable') {
        await post(acct(uid, 'disable'))
        toast.success('已禁用')
      } else {
        const r = await post<{ file_error?: string }>(acct(uid, 'remove'))
        toast.success(r.file_error ? '已移除（凭证文件删除失败：' + r.file_error + '）' : '已移除')
      }
    } catch (e) {
      toastError(e)
    } finally {
      setBusy(null)
      void qc.invalidateQueries({ queryKey: qk.overview })
    }
  }

  const run = (a: Account, action: AccountAction) => {
    if (action === 'tasks') return openTasks(a.uid)
    if (action === 'remove') {
      return ask({
        title: '移除账号「' + (a.nickname || a.uid.slice(0, 12)) + '」？',
        body: '移除账号将删除池状态与 auths/ 下的凭证文件，且不可恢复。',
        confirmLabel: '移除',
        onConfirm: () => void exec(a.uid, 'remove'),
      })
    }
    if (action === 'disable') {
      return ask({
        title: '禁用账号「' + (a.nickname || a.uid.slice(0, 12)) + '」？',
        body: '禁用后该账号不再参与选号，需手动解冻才能恢复。',
        confirmLabel: '禁用',
        onConfirm: () => void exec(a.uid, 'disable'),
      })
    }
    void exec(a.uid, action)
  }

  return { busy, run, confirmEl }
}
