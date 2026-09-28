// 所有读接口的查询（TanStack Query）。打上游的接口（模型、积分包、任务、券码）不随窗口聚焦自动重查，
// 只读网关内存的接口（总览、日志、队列）按页面需要轮询；标签页切到后台时轮询自动暂停。
import { QueryClient, keepPreviousData, useQuery } from '@tanstack/react-query'
import { ApiError, acct, api } from './api'
import type {
  ConfigResp, LogEntry, Model, Overview, PackageAccount, ProbesResp, QueueState, Task, UsageResp, VoucherAccount,
} from './types'

export const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // 401 由密钥框处理，重试没有意义；其余错误重试一次
      retry: (count, err) => !(err instanceof ApiError && err.status === 401) && count < 1,
      refetchOnWindowFocus: false,
    },
  },
})

export const qk = {
  overview: ['overview'] as const,
  logs: ['logs'] as const,
  models: ['models'] as const,
  probes: ['model_probes'] as const,
  usage: (hours: string) => ['usage', hours] as const,
  packages: ['packages'] as const,
  config: ['config'] as const,
  tasks: (uid: string) => ['tasks', uid] as const,
  queue: ['tasks-queue'] as const,
  vouchers: ['vouchers'] as const,
}

/** 总览（账号池）：账号池页 5 秒一刷，其余页面只给侧栏状态用，30 秒一刷 */
export function useOverview(intervalMs = 30_000) {
  return useQuery({
    queryKey: qk.overview,
    queryFn: () => api<Overview>('overview'),
    refetchInterval: intervalMs,
  })
}

export function useLogs(live: boolean) {
  return useQuery({
    queryKey: qk.logs,
    queryFn: () => api<{ entries: LogEntry[] | null }>('logs'),
    refetchInterval: live ? 5000 : false,
  })
}

/** 模型列表会实时查上游并刷新降级缓存：只在首次进入和手动「重新获取」时查 */
export function useModels() {
  return useQuery({
    queryKey: qk.models,
    queryFn: () => api<{ models: Model[] | null }>('models'),
    staleTime: Infinity,
  })
}

/** 实测上限是可选增强：读失败按「没有探测数据」处理，不影响模型列表 */
export function useProbes() {
  return useQuery({
    queryKey: qk.probes,
    queryFn: () => api<ProbesResp>('model_probes').catch((): ProbesResp => ({ probes: {}, exists: false })),
    staleTime: Infinity,
  })
}

export function useUsage(hours: string) {
  return useQuery({
    queryKey: qk.usage(hours),
    queryFn: () => api<UsageResp>('usage?hours=' + encodeURIComponent(hours)),
    placeholderData: keepPreviousData,
  })
}

/** 积分包逐账号查上游（慢）：一分钟内切回来直接用缓存，页面上有「刷新」 */
export function usePackages() {
  return useQuery({
    queryKey: qk.packages,
    queryFn: () => api<{ accounts: PackageAccount[] | null }>('packages'),
    staleTime: 60_000,
  })
}

export function useConfig() {
  return useQuery({
    queryKey: qk.config,
    queryFn: () => api<ConfigResp>('config'),
  })
}

export function useTasks(uid: string | null) {
  return useQuery({
    queryKey: qk.tasks(uid ?? ''),
    queryFn: () => api<{ tasks: Task[] | null }>(acct(uid!, 'tasks')),
    enabled: !!uid,
  })
}

/** 任务队列：执行中每 3 秒轮询一次，结束后停 */
export function useQueue() {
  return useQuery({
    queryKey: qk.queue,
    queryFn: () => api<QueueState>('tasks/queue'),
    refetchInterval: (q) => (q.state.data?.running ? 3000 : false),
  })
}

export function useVouchers(enabled: boolean) {
  return useQuery({
    queryKey: qk.vouchers,
    queryFn: () => api<{ accounts: VoucherAccount[] | null }>('school/vouchers'),
    enabled,
  })
}
