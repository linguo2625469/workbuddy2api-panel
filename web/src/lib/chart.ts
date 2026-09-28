// Recharts 自定义提示框收到的参数（只用到这两项；写成宽类型，免得和 Recharts 的泛型参数较劲）
export interface TipProps {
  active?: boolean
  payload?: readonly { payload?: unknown }[]
}
