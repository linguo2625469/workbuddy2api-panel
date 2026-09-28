import { useEffect, useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { toast } from '@heroui/react'
import { errorMessage } from './api'
import type { LogChannel } from './logs'

/** 每隔 intervalMs 返回一次当前时间（冷却倒计时逐秒走） */
export function useNow(intervalMs = 1000) {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), intervalMs)
    return () => clearInterval(t)
  }, [intervalMs])
  return now
}

/**
 * 后台批量任务（全部签到、旅行巡检…）已开始的提示：带一个「查看日志」按钮，
 * 点了跳到运行日志页并筛到对应频道。
 */
export function useStartedToast() {
  const navigate = useNavigate()
  return (title: string, ch: LogChannel = 'task') => {
    const id = toast.success(title, {
      description: '在后台执行，结果写在运行日志里',
      actionProps: {
        children: '查看日志',
        variant: 'tertiary',
        onPress: () => {
          toast.close(id)
          void navigate({ to: '/logs', search: { ch } })
        },
      },
    })
  }
}

export const toastError = (e: unknown, prefix = '') => toast.danger(prefix + errorMessage(e))
