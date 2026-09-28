// 用量时序图的数据整理（纯函数，见 usage.test.ts）。
// x 轴是真实时间轴，不按序号等距：没请求的时段不产生桶，1 小时和 8 小时的间隔必须画得不一样宽，
// 否则「什么时候用的」会失真。后端返回的是本地时区时间，这里按本地时间解析。
import type { UsagePoint } from './types'

export interface ChartPoint {
  t: number
  raw: string
  scope: 'hour' | 'day'
  prompt: number
  completion: number
  total: number
  requests: number
}

/** hour: "2026-09-16T13"，day: "2026-09-16"（日桶按当天 00:00 定位） */
export function parsePointTime(t: string): number | null {
  const s = t.length === 13 ? t + ':00:00' : t + 'T00:00:00'
  const ms = new Date(s).getTime()
  return Number.isNaN(ms) ? null : ms
}

export function chartPoints(series: UsagePoint[]): ChartPoint[] {
  const out: ChartPoint[] = []
  for (const p of series) {
    const t = parsePointTime(p.t)
    if (t === null) continue // 解析不出来的点丢掉，别让 NaN 传染整张图
    const prompt = Number(p.prompt_tokens || 0), completion = Number(p.completion_tokens || 0)
    out.push({ t, raw: p.t, scope: p.scope, prompt, completion, total: Number(p.total_tokens || 0) || prompt + completion, requests: p.requests || 0 })
  }
  return out.sort((a, b) => a.t - b.t)
}

/** 相邻两点的最小间隔（柱宽和两端留白按它算） */
export function minGap(pts: ChartPoint[]): number {
  let g = Infinity
  for (let i = 1; i < pts.length; i++) g = Math.min(g, pts[i].t - pts[i - 1].t)
  return Number.isFinite(g) && g > 0 ? g : 3600_000
}

/** x 轴刻度：按真实时间等距取至多 6 个位置，每个位置取最近的实际柱子，标签永远落在有数据的点上 */
export function nearestTicks(pts: ChartPoint[], count = 6): number[] {
  if (!pts.length) return []
  const t0 = pts[0].t, t1 = pts[pts.length - 1].t, span = Math.max(1, t1 - t0)
  const n = Math.min(count, pts.length)
  const out: number[] = []
  for (let k = 0; k < n; k++) {
    const target = t0 + span * (n === 1 ? 0.5 : k / (n - 1))
    let best = pts[0].t
    for (const p of pts) if (Math.abs(p.t - target) < Math.abs(best - target)) best = p.t
    if (!out.includes(best)) out.push(best)
  }
  return out
}

/** 跨天处画一条分隔线，长窗口里能看出日界 */
export function dayBoundaries(pts: ChartPoint[]): number[] {
  const out: number[] = []
  for (let i = 1; i < pts.length; i++) {
    if (new Date(pts[i].t).getDate() !== new Date(pts[i - 1].t).getDate()) out.push(pts[i].t)
  }
  return out
}

export function tickLabel(p: Pick<ChartPoint, 't' | 'scope'>): string {
  const d = new Date(p.t)
  return p.scope === 'day'
    ? d.getMonth() + 1 + '-' + String(d.getDate()).padStart(2, '0')
    : String(d.getHours()).padStart(2, '0') + ':00'
}
