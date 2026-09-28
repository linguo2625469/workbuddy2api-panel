// 配置：直接改 config.json。能热生效的字段保存即生效，装配期字段（监听地址、超时等）标「需重启」。
// 写入是深合并 + 原子替换，表单只提交它管理的键，未知键原样保留。
import { useMemo, useState } from 'react'
import { Eye, EyeSlash } from '@gravity-ui/icons'
import {
  Button, Card, Chip, Description, FieldError, Input, InputGroup, Label, ListBox, NumberField, Select, Switch, Tabs,
  TextField, toast,
} from '@heroui/react'
import { useQueryClient } from '@tanstack/react-query'
import { useBlocker } from '@tanstack/react-router'
import { post, setKey } from '../../lib/api'
import {
  ALL_FIELDS, CONFIG_TABS, changedFields, fromForm, tabOfField, toForm, validateField, type FieldDef, type FormValues,
} from '../../lib/config'
import { toastError } from '../../lib/hooks'
import { qk, useConfig } from '../../lib/queries'
import type { SaveConfigResp } from '../../lib/types'
import { BusyButton, Confirm, Loaded } from '../../components/Feedback'

export function ConfigPage() {
  const query = useConfig()
  return (
    <Loaded query={query} rows={8}>
      {(d) => <ConfigForm path={d.path} config={d.config} />}
    </Loaded>
  )
}

function ConfigForm({ path, config }: { path?: string; config: Record<string, unknown> }) {
  const qc = useQueryClient()
  const base = useMemo(() => toForm(config), [config])
  const [edits, setEdits] = useState<FormValues>({})
  const [tab, setTab] = useState('service')
  const [saving, setSaving] = useState(false)
  const values = { ...base, ...edits }
  const changed = changedFields(base, values)
  const errors = Object.fromEntries(ALL_FIELDS.map((f) => [f.name, validateField(f, values[f.name])]))
  const set = (name: string, v: string | boolean) => setEdits((e) => ({ ...e, [name]: v }))

  // 有没保存的改动时离开配置页，先问一句
  const blocker = useBlocker({ shouldBlockFn: () => changed.length > 0, withResolver: true, enableBeforeUnload: () => changed.length > 0 })

  const save = async () => {
    const bad = ALL_FIELDS.find((f) => errors[f.name])
    if (bad) {
      setTab(tabOfField(bad.name) ?? tab)
      toast.danger('「' + bad.label + '」' + errors[bad.name])
      return
    }
    setSaving(true)
    try {
      const r = await post<SaveConfigResp>('config', fromForm(values))
      const n = (r.restart_required || []).length
      toast.success(n ? '配置已保存，其中 ' + n + ' 项需重启进程生效' : '配置已保存并立即生效')
      // 密钥可能改了：本页后续请求沿用新值，避免下一次轮询被 401
      const k = String(values.api_key ?? '').trim()
      if (k) setKey(k)
      setEdits({})
      void qc.invalidateQueries({ queryKey: qk.config })
      void qc.invalidateQueries({ queryKey: qk.overview })
    } catch (e) {
      toastError(e, '保存失败：')
    } finally {
      setSaving(false)
    }
  }

  const changedByTab = (id: string) => changed.filter((n) => tabOfField(n) === id).length
  const restartChanged = changed.filter((n) => ALL_FIELDS.find((f) => f.name === n)?.restart).length

  return (
    <form className="flex flex-col gap-4" onSubmit={(e) => { e.preventDefault(); void save() }}>
      {path && <p className="text-sm text-muted">配置文件：<span className="font-mono">{path}</span></p>}
      <Tabs selectedKey={tab} onSelectionChange={(k) => setTab(String(k))}>
        <Tabs.ListContainer>
          <Tabs.List aria-label="配置分组">
            {CONFIG_TABS.map((t) => (
              <Tabs.Tab key={t.id} id={t.id} className="whitespace-nowrap">
                {t.label}
                {changedByTab(t.id) > 0 && <Chip size="sm" variant="soft" color="accent" className="ml-1.5">{changedByTab(t.id)}</Chip>}
                <Tabs.Indicator />
              </Tabs.Tab>
            ))}
          </Tabs.List>
        </Tabs.ListContainer>
        {CONFIG_TABS.map((t) => (
          <Tabs.Panel key={t.id} id={t.id} className="pt-4">
            <div className={`grid gap-4 ${t.id === 'schedule' ? 'md:grid-cols-2' : ''}`}>
              {t.sections.map((s, i) => (
                <Card key={s.title ?? i} className="gap-4">
                  {s.title && <Card.Header><Card.Title>{s.title}</Card.Title></Card.Header>}
                  <Card.Content className={`grid gap-x-5 gap-y-4 ${t.id === 'schedule' ? '' : 'md:grid-cols-2 xl:grid-cols-3'}`}>
                    {s.fields.map((f) => (
                      <Field key={f.name} f={f} value={values[f.name]} error={errors[f.name]} onChange={(v) => set(f.name, v)} />
                    ))}
                  </Card.Content>
                </Card>
              ))}
            </div>
            {t.id === 'upstream' && (
              <p className="mt-3 text-sm text-muted">Upstash Redis 镜像、凭证目录与状态文件路径需手工编辑配置文件（判为重启项）。</p>
            )}
          </Tabs.Panel>
        ))}
      </Tabs>

      <Card className="sticky bottom-3 z-30 flex-row flex-wrap items-center gap-3 p-3 shadow-lg">
        <span className="text-sm text-muted">
          {changed.length === 0 ? '没有改动' : `改了 ${changed.length} 项`}
          {restartChanged > 0 && <span className="text-warning">，其中 {restartChanged} 项需重启进程生效</span>}
        </span>
        <div className="ml-auto flex gap-2">
          <Button variant="secondary" isDisabled={changed.length === 0 || saving} onPress={() => setEdits({})}>放弃修改</Button>
          <BusyButton type="submit" variant="primary" busy={saving} isDisabled={changed.length === 0}>保存配置</BusyButton>
        </div>
      </Card>

      <Confirm
        open={blocker.status === 'blocked'}
        onClose={() => blocker.reset?.()}
        title="有未保存的配置改动"
        body={`改了 ${changed.length} 项还没保存，离开后这些改动会丢失。`}
        confirmLabel="放弃改动并离开"
        onConfirm={() => blocker.proceed?.()}
      />
    </form>
  )
}

function FieldLabel({ f }: { f: FieldDef }) {
  return (
    <Label className="flex items-center gap-1.5">
      {f.label}
      {f.restart && <Chip size="sm" variant="soft" color="warning">需重启</Chip>}
    </Label>
  )
}

function Field({ f, value, error, onChange }: {
  f: FieldDef
  value: string | boolean
  error: string | null
  onChange: (v: string | boolean) => void
}) {
  const [reveal, setReveal] = useState(false)

  if (f.kind === 'switch') {
    return (
      <Switch isSelected={!!value} onChange={onChange}>
        <Switch.Content>
          <Switch.Control><Switch.Thumb /></Switch.Control>
          <Label>{f.label}</Label>
        </Switch.Content>
        {f.hint && <Description>{f.hint}</Description>}
      </Switch>
    )
  }

  const text = String(value ?? '')

  if (f.kind === 'select') {
    return (
      <Select value={text || null} onChange={(v) => v != null && onChange(String(v))} className="w-full">
        <FieldLabel f={f} />
        <Select.Trigger>
          <Select.Value />
          <Select.Indicator />
        </Select.Trigger>
        {f.hint && <Description>{f.hint}</Description>}
        <Select.Popover>
          <ListBox>
            {f.options!.map((o) => (
              <ListBox.Item key={o.id} id={o.id} textValue={o.label}>{o.label}<ListBox.ItemIndicator /></ListBox.Item>
            ))}
          </ListBox>
        </Select.Popover>
      </Select>
    )
  }

  if (f.kind === 'number') {
    return (
      <NumberField
        value={text === '' ? Number.NaN : Number(text)}
        onChange={(n) => onChange(Number.isNaN(n) ? '' : String(n))}
        minValue={f.min}
        step={f.step}
        isInvalid={!!error}
        className="w-full"
      >
        <FieldLabel f={f} />
        <NumberField.Group>
          <NumberField.DecrementButton />
          <NumberField.Input placeholder={f.placeholder} />
          <NumberField.IncrementButton />
        </NumberField.Group>
        {f.hint && !error && <Description>{f.hint}</Description>}
        <FieldError>{error}</FieldError>
      </NumberField>
    )
  }

  return (
    <TextField
      value={text}
      onChange={onChange}
      isInvalid={!!error}
      type={f.kind === 'password' && !reveal ? 'password' : 'text'}
      autoComplete="off"
      className="w-full"
    >
      <FieldLabel f={f} />
      {f.kind === 'password' ? (
        <InputGroup>
          <InputGroup.Input placeholder={f.placeholder} />
          <InputGroup.Suffix>
            <Button isIconOnly size="sm" variant="ghost" aria-label={reveal ? '隐藏' : '显示'} onPress={() => setReveal(!reveal)}>
              {reveal ? <EyeSlash /> : <Eye />}
            </Button>
          </InputGroup.Suffix>
        </InputGroup>
      ) : (
        <Input placeholder={f.placeholder} />
      )}
      {(f.hint || f.kind === 'hours') && !error && (
        <Description>{f.kind === 'hours' ? '整点小时（0–23），逗号分隔' + (f.hint ? '；' + f.hint : '') : f.hint}</Description>
      )}
      <FieldError>{error}</FieldError>
    </TextField>
  )
}
