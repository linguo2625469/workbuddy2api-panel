// 模型列表的筛选与实测上限关联（纯函数，见 models.test.ts）。
import type { Model, Probe } from './types'

export type RealmFilter = 'all' | 'cn' | 'global'
export type Capability = 'tools' | 'vision' | 'effort'

/** 模型 id 带域前缀（cn:glm-5.2 / global:gpt-5），就是调用时要填的完整 model 值 */
export const modelRealm = (id: string) => (id.startsWith('global:') ? 'global' : id.startsWith('cn:') ? 'cn' : '')

export function hasCapability(m: Model, c: Capability): boolean {
  if (c === 'tools') return !!m.supports_tool_call
  if (c === 'vision') return !!m.supports_images
  return (m.supported_efforts?.length ?? 0) > 0
}

export function filterModels(list: Model[], q: string, realm: RealmFilter, caps: Capability[]): Model[] {
  const kw = q.trim().toLowerCase()
  return list.filter((m) => {
    if (realm !== 'all' && modelRealm(m.id) !== realm) return false
    if (caps.some((c) => !hasCapability(m, c))) return false
    return !kw || [m.id, m.name, m.vendor].some((s) => (s || '').toLowerCase().includes(kw))
  })
}

/** 探测键带域前缀（cn:glm-5.2），按「精确命中或 :后缀」关联到模型（与旧版口径一致） */
export function probeFor(probes: Record<string, Probe>, id: string): Probe | undefined {
  if (probes[id]) return probes[id]
  const key = Object.keys(probes).find((k) => k.endsWith(':' + id))
  return key ? probes[key] : undefined
}
