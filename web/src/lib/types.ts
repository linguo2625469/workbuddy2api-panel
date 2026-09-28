// 面板接口（/panel/api/*）的返回类型，手写对齐 internal/panel 与 internal/pool 的 JSON 字段。
// Go 端 time.Time 的零值序列化为 "0001-01-01T00:00:00Z"，展示前要当作「无」处理（见 format.ts 的 isZeroTime）。

export interface TokenUsage {
  request_count?: number
  usage_count?: number
  prompt_tokens?: number
  completion_tokens?: number
  total_tokens?: number
  last_latency_ms?: number
  last_tokens_per_second?: number
  last_used_at?: string
  last_model?: string
}

export interface ModelCost {
  model: string
  /** 实测每千 token 单价；≤0 = 实测免费 */
  cost_per_1k: number
  last_seen: string
  samples?: number
}

/** 模型级限额台账（issue #36）：该账号的某个模型还在限额中，预计何时恢复 */
export interface RateLimitedModel {
  model: string
  until?: string
  reset_at?: string
  reason?: string
}

export interface Account {
  uid: string
  nickname?: string
  credits: number | null
  credits_total?: number
  credits_expiring?: number
  credits_earliest_expiry?: string
  credits_earliest_remaining?: number
  cooling: boolean
  cool_kind?: string
  cool_remaining_sec?: number
  until?: string
  reason?: string
  soft_streak?: number
  rate_limited_models?: RateLimitedModel[]
  realm?: string
  disabled: boolean
  disabled_reason?: string
  success_count?: number
  err_total?: number
  last_success?: string
  last_err?: string
  token_usage?: TokenUsage
  model_costs?: ModelCost[]
  consecutive_fails: number
  degrade_until?: string
  in_flight: number
  breaker_fails: number
  breaker_until?: string
}

export interface Overview {
  version: string
  uptime_sec: number
  auth_required: boolean
  redis_mode: string
  sticky_sessions: number
  total: number
  healthy: number
  cooling: number
  disabled: number
  in_flight_full: number
  accounts: Account[] | null
}

export interface LogEntry {
  ts: string
  /** task / chat / sys */
  ch: string
  text: string
}

export interface Model {
  id: string
  name?: string
  default_effort?: string
  supported_efforts?: string[] | null
  can_disable_thinking?: boolean
  supports_reasoning?: boolean
  supports_images?: boolean
  supports_tool_call?: boolean
  only_reasoning?: boolean
  credits?: string | number
  description?: string
  vendor?: string
  tags?: string[] | null
  is_default?: boolean
  promo_factor?: number
  promo_credits?: string | number
  promo_label?: string
  promo_note?: string
  context_length?: number
  max_output_tokens?: number
}

export interface Probe {
  claimed?: number
  measured?: number
  /** clamped（被钳制）/ at_least（至少）/ 其他 = 未测出 */
  verdict?: string
  note?: string
  tested_at?: string
}

export interface ProbesResp {
  probes: Record<string, Probe>
  exists: boolean
  updated_at?: string
}

export interface UsageAgg {
  requests: number
  errors: number
  prompt_tokens: number
  completion_tokens: number
  total_tokens: number
  avg_latency_ms: number
  avg_tokens_per_second: number
}

export interface KeyedUsage extends UsageAgg {
  key: string
  realm?: string
  /** 按账号的行里放昵称 */
  extra?: string
}

export interface UsagePoint extends UsageAgg {
  /** hour: "2026-09-16T13"，day: "2026-09-16" */
  t: string
  scope: 'hour' | 'day'
}

export interface UsageResp {
  totals: UsageAgg
  by_realm: KeyedUsage[] | null
  by_account: KeyedUsage[] | null
  by_model: KeyedUsage[] | null
  series: UsagePoint[] | null
  buckets: number
  file_bytes: number
  since?: string
  generated: string
}

export interface CreditPackage {
  name: string
  remain: number
  used: number
  size: number
  end_time?: string
  /** 毫秒时间戳 */
  expires_at?: number
  created_at?: string
  package_code?: string
  sub_product_code?: string
  sub_product_name?: string
  cycle?: boolean
}

export interface PackageAccount {
  uid: string
  nickname: string
  realm: string
  remain: number
  size: number
  packages: CreditPackage[] | null
  error?: string
}

export interface Task {
  task_code: string
  title?: string
  description?: string
  task_desc?: string
  credit?: number
  energy?: number
  reward_buddy?: boolean
  tag?: string
  jump_url?: string
  locked?: boolean
  target: number
  current: number
  accept_status?: string
  claimable?: boolean
  claimed?: boolean
}

/** 单个任务「一键完成」的结果 */
export interface AutoResult {
  ok?: boolean
  skipped?: boolean
  message?: string
  progress_before?: string
  progress_after?: string
  claimable?: boolean
  claimed?: boolean
  claim_error?: string
  attempt?: boolean
}

/** 「一键完成可自动任务」逐项结果 */
export interface AutoAllItem {
  task_code: string
  desc?: string
  /** done / skipped / error */
  status: string
  message?: string
  progress_after?: string
  claimed?: boolean
  claim_error?: string
}

export interface AcceptAllResp {
  accepted?: number
  failed?: string[] | null
  message?: string
}

export interface ScanAccount {
  uid: string
  nickname: string
  growth?: Task[]
  growth_error?: string
}

export interface QueueItem {
  uid: string
  nickname: string
  kind: string
  code: string
  /** pending / running / done / skipped / error */
  status: string
  message?: string
}

export interface QueueState {
  running: boolean
  total: number
  conc: number
  started: boolean
  started_at: string
  seq: number
  items: QueueItem[] | null
}

export interface RunQueueResp {
  started: boolean
  message?: string
  total?: number
  seq?: number
}

export interface Voucher {
  prize_name?: string
  sku_code?: string
  code: string
  valid_to?: string
  granted_at?: string
}

export interface VoucherAccount {
  uid: string
  nickname: string
  vouchers: Voucher[] | null
  error?: string
}

export interface ConfigResp {
  path?: string
  config: Record<string, unknown>
}

export interface SaveConfigResp {
  restart_required?: string[] | null
}

export interface LoginStart {
  url: string
  state: string
  realm: string
}

export interface LoginPoll {
  done: boolean
  message?: string
  uid?: string
  nickname?: string
  realm?: string
  credits?: number
  credits_total?: number
}

export interface ImportResp {
  total: number
  imported: number
  skipped: number
  errors: string[] | null
}
