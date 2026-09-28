import { useState, type ReactNode } from 'react'
import { Confirm } from './Feedback'

interface Ask {
  title: ReactNode
  body: ReactNode
  confirmLabel?: string
  danger?: boolean
  onConfirm: () => void
}

/**
 * 确认框：const [confirmEl, ask] = useConfirm(); ask({ title, body, onConfirm })，并把 confirmEl 放进页面。
 * 关闭时保留上一次的内容，退场动画里文字不会先消失。
 */
export function useConfirm() {
  const [open, setOpen] = useState(false)
  const [req, setReq] = useState<Ask>({ title: '', body: '', onConfirm: () => {} })
  const el = <Confirm open={open} onClose={() => setOpen(false)} {...req} />
  const ask = (r: Ask) => {
    setReq(r)
    setOpen(true)
  }
  return [el, ask] as const
}
