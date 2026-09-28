import type { ReactNode } from 'react'
import { CircleInfo } from '@gravity-ui/icons'
import { Button, Card, Popover } from '@heroui/react'

/** 页面里的一块：标题 + 说明 + 右侧操作 + 内容 */
export function Panel({ title, desc, actions, children, className = '', contentClassName }: {
  title?: ReactNode
  desc?: ReactNode
  actions?: ReactNode
  children?: ReactNode
  className?: string
  contentClassName?: string
}) {
  return (
    <Card className={className}>
      {(title || actions) && (
        <Card.Header className="flex flex-row flex-wrap items-center gap-x-3 gap-y-2">
          <div className="min-w-0">
            {title && <Card.Title>{title}</Card.Title>}
            {desc && <Card.Description>{desc}</Card.Description>}
          </div>
          {actions && <div className="ml-auto flex flex-wrap items-center gap-2">{actions}</div>}
        </Card.Header>
      )}
      {children !== undefined && <Card.Content className={contentClassName}>{children}</Card.Content>}
    </Card>
  )
}

/** 统计数字 */
export function Stat({ label, value, tone }: { label: ReactNode; value: ReactNode; tone?: 'success' | 'warning' | 'danger' }) {
  const color = tone === 'success' ? 'text-success' : tone === 'warning' ? 'text-warning' : tone === 'danger' ? 'text-danger' : ''
  return (
    <Card className="gap-1 p-4">
      <span className={`text-2xl font-semibold tabular-nums ${color}`}>{value}</span>
      <span className="text-xs text-muted">{label}</span>
    </Card>
  )
}

/**
 * 点一下看说明（手机上没有悬停，原先藏在 title 悬停提示里的内容都改用它）。
 */
export function InfoTip({ label = '查看说明', children }: { label?: string; children: ReactNode }) {
  return (
    <Popover>
      <Button isIconOnly aria-label={label} size="sm" variant="ghost" className="size-6 min-w-6 shrink-0 text-muted">
        <CircleInfo className="size-3.5" />
      </Button>
      <Popover.Content className="max-w-xs">
        <Popover.Dialog>
          <div className="whitespace-pre-line text-sm leading-6">{children}</div>
        </Popover.Dialog>
      </Popover.Content>
    </Popover>
  )
}
