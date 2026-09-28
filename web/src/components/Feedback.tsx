// 各页面共用的加载、出错、空状态和确认框，统一口径：
// 加载中用骨架占位；出错说清原因并给「重试」；已有数据时刷新失败不清空页面（只提示）。
import type { ComponentProps, ReactNode } from 'react'
import { Alert, AlertDialog, Button, EmptyState, Skeleton, Spinner } from '@heroui/react'
import type { UseQueryResult } from '@tanstack/react-query'

export function Loading({ rows = 3, className = '' }: { rows?: number; className?: string }) {
  return (
    <div className={`flex flex-col gap-3 ${className}`} aria-busy="true" aria-label="加载中">
      {Array.from({ length: rows }, (_, i) => <Skeleton key={i} className="h-10 w-full rounded-xl" />)}
    </div>
  )
}

export function ErrorBox({ error, onRetry, title = '读取失败' }: { error: unknown; onRetry?: () => void; title?: string }) {
  return (
    <Alert status="danger">
      <Alert.Indicator />
      <Alert.Content>
        <Alert.Title>{title}</Alert.Title>
        <Alert.Description>{error instanceof Error ? error.message : typeof error === 'string' ? error : '未知错误'}</Alert.Description>
      </Alert.Content>
      {onRetry && <Button size="sm" variant="secondary" onPress={onRetry}>重试</Button>}
    </Alert>
  )
}

export function Empty({ title, children, action, className = '' }: { title: ReactNode; children?: ReactNode; action?: ReactNode; className?: string }) {
  return (
    <EmptyState className={`flex flex-col items-center gap-1.5 px-4 py-10 text-center ${className}`}>
      <p className="font-medium">{title}</p>
      {children && <p className="max-w-md text-sm text-muted">{children}</p>}
      {action && <div className="mt-3">{action}</div>}
    </EmptyState>
  )
}

/**
 * 一个查询的三种状态：还没数据时加载中 / 出错；有数据就渲染（后台刷新失败时保留旧数据，只在顶部提示）。
 */
export function Loaded<T>({ query, rows, children }: { query: UseQueryResult<T>; rows?: number; children: (data: T) => ReactNode }) {
  if (query.data === undefined) {
    if (query.error) return <ErrorBox error={query.error} onRetry={() => void query.refetch()} />
    return <Loading rows={rows} />
  }
  return (
    <>
      {query.error && <ErrorBox title="刷新失败，下面是上一次的数据" error={query.error} onRetry={() => void query.refetch()} />}
      {children(query.data)}
    </>
  )
}

/** 提交中转圈的按钮 */
export function BusyButton({ busy, children, ...rest }: ComponentProps<typeof Button> & { busy?: boolean; children?: ReactNode }) {
  return (
    <Button isPending={busy} {...rest}>
      {busy && <Spinner color="current" size="sm" />}
      {children}
    </Button>
  )
}

interface ConfirmProps {
  open: boolean
  onClose: () => void
  title: ReactNode
  body: ReactNode
  confirmLabel?: string
  onConfirm: () => void
  danger?: boolean
}

/** 危险操作（移除、禁用、长耗时批量任务）前问一句 */
export function Confirm({ open, onClose, title, body, confirmLabel = '确认', onConfirm, danger = true }: ConfirmProps) {
  return (
    <AlertDialog.Backdrop isOpen={open} onOpenChange={(v) => { if (!v) onClose() }}>
      <AlertDialog.Container>
        <AlertDialog.Dialog className="sm:max-w-[440px]">
          <AlertDialog.Header>
            <AlertDialog.Icon status={danger ? 'danger' : 'accent'} />
            <AlertDialog.Heading>{title}</AlertDialog.Heading>
          </AlertDialog.Header>
          <AlertDialog.Body><div className="text-sm leading-6 text-muted">{body}</div></AlertDialog.Body>
          <AlertDialog.Footer>
            <Button variant="tertiary" onPress={onClose}>取消</Button>
            <Button variant={danger ? 'danger' : 'primary'} onPress={() => { onConfirm(); onClose() }}>{confirmLabel}</Button>
          </AlertDialog.Footer>
        </AlertDialog.Dialog>
      </AlertDialog.Container>
    </AlertDialog.Backdrop>
  )
}
