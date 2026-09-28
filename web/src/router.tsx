// 路由：hash 模式（#/accounts），Go 端只需要提供 /panel/ 一个页面，不用做前端路由回落。
// 用图表的两个页面（用量、积分构成）按需加载，Recharts 不进首屏。
import {
  Navigate, createHashHistory, createRootRoute, createRoute, createRouter, lazyRouteComponent,
} from '@tanstack/react-router'
import { Shell } from './components/Shell'
import { isLogChannel, type LogChannel } from './lib/logs'
import { AccountsPage } from './pages/accounts/AccountsPage'
import { ConfigPage } from './pages/config/ConfigPage'
import { LogsPage } from './pages/logs/LogsPage'
import { ModelsPage } from './pages/models/ModelsPage'
import { TaskCenterPage } from './pages/taskcenter/TaskCenterPage'

const rootRoute = createRootRoute({
  component: Shell,
  notFoundComponent: () => <Navigate to="/accounts" replace />,
})

// 每个路由单独 createRoute（路径要保持字面量类型，封装成接收 string 的辅助函数会让整棵路由树的类型退化成 any）
const indexRoute = createRoute({ getParentRoute: () => rootRoute, path: '/', component: () => <Navigate to="/accounts" replace /> })
const accountsRoute = createRoute({ getParentRoute: () => rootRoute, path: '/accounts', component: AccountsPage, staticData: { title: '账号池' } })
const usageRoute = createRoute({
  getParentRoute: () => rootRoute, path: '/usage', staticData: { title: '用量' },
  component: lazyRouteComponent(() => import('./pages/usage/UsagePage'), 'UsagePage'),
})
const packagesRoute = createRoute({
  getParentRoute: () => rootRoute, path: '/packages', staticData: { title: '积分构成' },
  component: lazyRouteComponent(() => import('./pages/packages/PackagesPage'), 'PackagesPage'),
})
const taskCenterRoute = createRoute({ getParentRoute: () => rootRoute, path: '/taskscenter', component: TaskCenterPage, staticData: { title: '任务中心' } })
const modelsRoute = createRoute({ getParentRoute: () => rootRoute, path: '/models', component: ModelsPage, staticData: { title: '模型与档位' } })
const configRoute = createRoute({ getParentRoute: () => rootRoute, path: '/config', component: ConfigPage, staticData: { title: '配置' } })
const logsRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/logs',
  component: LogsPage,
  staticData: { title: '运行日志' },
  validateSearch: (s: Record<string, unknown>): { ch?: LogChannel } => (isLogChannel(s.ch) ? { ch: s.ch } : {}),
})

const routeTree = rootRoute.addChildren([
  indexRoute, accountsRoute, usageRoute, packagesRoute, taskCenterRoute, modelsRoute, configRoute, logsRoute,
])

// 旧版面板的地址是 #accounts、#usage…（没有斜杠），书签打开时换成新格式
const legacy = /^#([a-z]+)$/.exec(location.hash)
if (legacy) history.replaceState(null, '', '#/' + legacy[1])

export const router = createRouter({ routeTree, history: createHashHistory() })

declare module '@tanstack/react-router' {
  interface Register {
    router: typeof router
  }
  interface StaticDataRouteOption {
    title?: string
  }
}
