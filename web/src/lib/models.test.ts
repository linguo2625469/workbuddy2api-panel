import { describe, expect, it } from 'vitest'
import { filterModels, probeFor } from './models'
import type { Model } from './types'

const list: Model[] = [
  { id: 'cn:glm-5.2', name: 'GLM 5.2', vendor: 'zhipu', supports_tool_call: true, supported_efforts: ['low', 'high'] },
  { id: 'cn:hunyuan', name: '混元', supports_images: true },
  { id: 'global:gpt-5', name: 'GPT-5', vendor: 'openai', supports_tool_call: true, supports_images: true },
]

describe('models', () => {
  it('按域、能力、关键字筛选', () => {
    expect(filterModels(list, '', 'cn', []).map((m) => m.id)).toEqual(['cn:glm-5.2', 'cn:hunyuan'])
    expect(filterModels(list, '', 'all', ['tools', 'vision']).map((m) => m.id)).toEqual(['global:gpt-5'])
    expect(filterModels(list, '', 'all', ['effort']).map((m) => m.id)).toEqual(['cn:glm-5.2'])
    expect(filterModels(list, 'OPENAI', 'all', []).map((m) => m.id)).toEqual(['global:gpt-5'])
  })

  it('实测上限按精确键或 :后缀 关联', () => {
    const probes = { 'cn:glm-5.2': { measured: 1 }, 'global:gpt-5': { measured: 2 } }
    expect(probeFor(probes, 'cn:glm-5.2')?.measured).toBe(1)
    expect(probeFor(probes, 'gpt-5')?.measured).toBe(2)
    expect(probeFor({ 'global:glm-5.2': { measured: 3 } }, 'cn:glm-5.2')).toBeUndefined()
    expect(probeFor(probes, 'cn:hunyuan')).toBeUndefined()
  })
})
