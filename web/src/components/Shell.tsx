// 外壳：左侧导航（窄屏收进抽屉）+ 顶栏（页面标题、运行时长、明暗切换、刷新余额、添加账号）。
// HeroUI 没有侧栏/顶栏组件，布局用 Tailwind 搭，里面的导航、按钮、抽屉都是 HeroUI 组件。
import { useEffect, useState, type ComponentType, type SVGProps } from 'react'
import {
  ArrowsRotateRight, Bars, Boxes3, ChartColumn, Cpu, Gear, ListCheck, ListTimeline, Moon, Persons, Plus, Sun,
} from '@gravity-ui/icons'
import {
  Button, Drawer, Label, ListBox, RouterProvider, ScrollShadow, Toast, Tooltip, toast, useMediaQuery, useTheme,
} from '@heroui/react'
import { useQueryClient } from '@tanstack/react-query'
import { Outlet, useLocation, useMatches, useRouter } from '@tanstack/react-router'
import { post } from '../lib/api'
import { uptime } from '../lib/format'
import { toastError } from '../lib/hooks'
import { qk, useOverview } from '../lib/queries'
import { AddAccountModal } from './AddAccountModal'
import { BusyButton } from './Feedback'
import { KeyGate } from './KeyGate'

type Icon = ComponentType<SVGProps<SVGSVGElement>>

const NAV: { to: string; label: string; icon: Icon }[] = [
  { to: '/accounts', label: '账号池', icon: Persons },
  { to: '/usage', label: '用量', icon: ChartColumn },
  { to: '/packages', label: '积分构成', icon: Boxes3 },
  { to: '/taskscenter', label: '任务中心', icon: ListCheck },
  { to: '/models', label: '模型与档位', icon: Cpu },
  { to: '/config', label: '配置', icon: Gear },
  { to: '/logs', label: '运行日志', icon: ListTimeline },
]

export function Shell() {
  const router = useRouter()
  const isDesktop = useMediaQuery('(min-width: 1024px)')
  const [drawer, setDrawer] = useState(false)
  const [adding, setAdding] = useState(false)
  const overview = useOverview()
  const matches = useMatches()
  const title = matches.map((m) => m.staticData.title).filter(Boolean).pop() ?? ''

  useEffect(() => {
    document.title = (title ? title + ' · ' : '') + 'WorkBuddy2API'
  }, [title])

  return (
    <RouterProvider
      navigate={(to) => void router.navigate({ to })}
      useHref={(to) => router.history.createHref(router.buildLocation({ to }).href)}
    >
      <div className="flex min-h-dvh">
        <aside className="sticky top-0 hidden h-dvh w-56 shrink-0 flex-col border-r border-separator lg:flex">
          <Sidebar />
        </aside>
        <Drawer.Backdrop isOpen={drawer && !isDesktop} onOpenChange={setDrawer}>
          <Drawer.Content placement="left">
            <Drawer.Dialog aria-label="导航菜单" className="w-64 p-0">
              <Sidebar onNavigate={() => setDrawer(false)} />
            </Drawer.Dialog>
          </Drawer.Content>
        </Drawer.Backdrop>

        <div className="flex min-w-0 flex-1 flex-col">
          <header className="sticky top-0 z-40 flex h-14 shrink-0 items-center gap-2 border-b border-separator bg-background px-3 sm:gap-3 sm:px-6">
            <Button isIconOnly aria-label="打开菜单" variant="ghost" className="lg:hidden" onPress={() => setDrawer(true)}>
              <Bars />
            </Button>
            <h1 className="truncate text-base font-semibold sm:text-lg">{title}</h1>
            {overview.data && <span className="hidden truncate text-sm text-muted md:inline">{uptime(overview.data.uptime_sec)}</span>}
            <div className="ml-auto flex items-center gap-1.5 sm:gap-2">
              <ThemeSwitch />
              <RefreshBalance compact={!isDesktop} />
              <Button variant="primary" isIconOnly={!isDesktop} aria-label="添加账号" onPress={() => setAdding(true)}>
                <Plus />{isDesktop && '添加账号'}
              </Button>
            </div>
          </header>
          <main className="mx-auto flex w-full max-w-7xl flex-col gap-4 px-3 pb-16 pt-4 sm:px-6 sm:pt-6">
            <Outlet />
          </main>
        </div>
      </div>
      <AddAccountModal open={adding} onClose={() => setAdding(false)} />
      <KeyGate />
      <Toast.Provider placement={isDesktop ? 'bottom end' : 'top'} />
    </RouterProvider>
  )
}

function Sidebar({ onNavigate }: { onNavigate?: () => void }) {
  const { pathname } = useLocation()
  const { data } = useOverview()
  const state = !data ? { text: '连接中', dot: 'bg-default' }
    : data.healthy > 0 ? { text: '服务正常', dot: 'bg-success' }
    : data.total ? { text: '无可用账号', dot: 'bg-warning' }
    : { text: '待添加账号', dot: 'bg-danger' }

  return (
    <>
      <div className="px-5 pb-3 pt-5">
        <div className="font-semibold">WorkBuddy2API</div>
        <div className="mt-0.5 text-xs text-muted">{data ? 'v' + data.version : '控制台'}</div>
      </div>
      <ScrollShadow hideScrollBar className="flex-1 px-3">
        <ListBox aria-label="导航" selectionMode="none" onAction={onNavigate}>
          {NAV.map((item) => (
            <ListBox.Item
              key={item.to}
              id={item.to}
              href={item.to}
              textValue={item.label}
              className={pathname === item.to ? 'bg-default' : undefined}
            >
              <item.icon className="size-4.5 shrink-0 text-muted" />
              <Label className="font-normal">{item.label}</Label>
            </ListBox.Item>
          ))}
        </ListBox>
      </ScrollShadow>
      <div className="flex flex-col gap-1 border-t border-separator px-5 py-3 text-xs text-muted">
        <span className="flex items-center gap-2"><span className={`size-2 rounded-full ${state.dot}`} />{state.text}</span>
        {data && <span>v{data.version} · {data.redis_mode === 'upstash' ? 'Redis 镜像' : '本地内存'}</span>}
      </div>
    </>
  )
}

function ThemeSwitch() {
  const { resolvedTheme, setTheme } = useTheme('system')
  const isDark = resolvedTheme === 'dark'
  const label = isDark ? '切换到浅色' : '切换到深色'
  return (
    <Tooltip delay={0}>
      <Button isIconOnly aria-label={label} variant="ghost" onPress={() => setTheme(isDark ? 'light' : 'dark')}>
        {isDark ? <Sun /> : <Moon />}
      </Button>
      <Tooltip.Content><p>{label}</p></Tooltip.Content>
    </Tooltip>
  )
}

/** 从上游刷新全部账号余额（5 秒轮询只读内存，不打上游） */
function RefreshBalance({ compact }: { compact: boolean }) {
  const qc = useQueryClient()
  const [busy, setBusy] = useState(false)
  const run = async () => {
    setBusy(true)
    try {
      await post('balance_all')
      toast.success('余额已从上游刷新')
    } catch (e) {
      toastError(e, '刷新失败：')
    } finally {
      setBusy(false)
      void qc.invalidateQueries({ queryKey: qk.overview })
      void qc.invalidateQueries({ queryKey: qk.logs })
    }
  }
  return (
    <BusyButton variant="secondary" busy={busy} isIconOnly={compact} aria-label="刷新余额" onPress={() => void run()}>
      {!busy && <ArrowsRotateRight />}{!compact && '刷新余额'}
    </BusyButton>
  )
}
