// 添加账号：浏览器 OAuth 登录（免重启热加载进池）或导入 cockpit tools 导出的 JSON。
import { useEffect, useRef, useState } from 'react'
import { ArrowUpRightFromSquare, Copy } from '@gravity-ui/icons'
import {
  Alert, Button, Description, Label, Modal, Radio, RadioGroup, Spinner, Tabs, toast,
} from '@heroui/react'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { api, errorMessage, post, upload } from '../lib/api'
import { copyText } from '../lib/clipboard'
import { qk } from '../lib/queries'
import type { ImportResp, LoginPoll, LoginStart } from '../lib/types'
import { BusyButton } from './Feedback'

export function AddAccountModal({ open, onClose }: { open: boolean; onClose: () => void }) {
  return (
    <Modal.Backdrop isOpen={open} onOpenChange={(v) => { if (!v) onClose() }}>
      <Modal.Container size="md" placement="auto">
        <Modal.Dialog>
          <Modal.CloseTrigger />
          <Modal.Header>
            <Modal.Heading>添加账号</Modal.Heading>
          </Modal.Header>
          {/* 每次打开都是新的一轮：关闭时卸载内容，登录轮询随之停止 */}
          {open && <AddAccountBody onClose={onClose} />}
        </Modal.Dialog>
      </Modal.Container>
    </Modal.Backdrop>
  )
}

function AddAccountBody({ onClose }: { onClose: () => void }) {
  return (
    <Modal.Body>
      <Tabs>
        <Tabs.ListContainer>
          <Tabs.List aria-label="添加方式">
            <Tabs.Tab id="login">浏览器登录<Tabs.Indicator /></Tabs.Tab>
            <Tabs.Tab id="import">导入 JSON<Tabs.Indicator /></Tabs.Tab>
          </Tabs.List>
        </Tabs.ListContainer>
        <Tabs.Panel id="login" className="pt-4"><LoginPanel onClose={onClose} /></Tabs.Panel>
        <Tabs.Panel id="import" className="pt-4"><ImportPanel /></Tabs.Panel>
      </Tabs>
    </Modal.Body>
  )
}

function LoginPanel({ onClose }: { onClose: () => void }) {
  const qc = useQueryClient()
  const [realm, setRealm] = useState('cn')
  const [session, setSession] = useState<LoginStart | null>(null)
  const [starting, setStarting] = useState(false)
  const [startError, setStartError] = useState('')

  // 授权完成前每 3 秒查一次；完成或出错后停
  const poll = useQuery({
    queryKey: ['login-poll', session?.state],
    queryFn: () => api<LoginPoll>('login/poll?state=' + encodeURIComponent(session!.state)),
    enabled: !!session,
    retry: false,
    refetchInterval: (q) => (q.state.data?.done || q.state.error ? false : 3000),
  })
  const done = poll.data?.done ? poll.data : null

  useEffect(() => {
    if (!done) return
    void qc.invalidateQueries({ queryKey: qk.overview })
    const t = setTimeout(onClose, 1600)
    return () => clearTimeout(t)
  }, [done, qc, onClose])

  const start = async () => {
    setStarting(true)
    setStartError('')
    try {
      setSession(await post<LoginStart>('login/start', { realm }))
    } catch (e) {
      setStartError(errorMessage(e))
    } finally {
      setStarting(false)
    }
  }

  if (done) {
    const credits = done.credits != null && done.credits >= 0
      ? ' · 积分 ' + done.credits + (done.credits_total ? '/' + done.credits_total : '') : ''
    return (
      <Alert status="success">
        <Alert.Indicator />
        <Alert.Content>
          <Alert.Title>已添加 {done.nickname || done.uid}{done.realm === 'global' ? '（国际版）' : ''}</Alert.Title>
          <Alert.Description>账号已载入池中{credits}</Alert.Description>
        </Alert.Content>
      </Alert>
    )
  }

  if (session) {
    return (
      <div className="flex flex-col gap-3">
        <p className="text-sm">在浏览器打开以下链接并登录：</p>
        <code className="select-all break-all rounded-xl bg-surface-secondary px-3 py-2.5 text-xs text-accent">{session.url}</code>
        <div className="flex flex-wrap gap-2">
          <Button variant="primary" onPress={() => window.open(session.url, '_blank', 'noopener')}>
            <ArrowUpRightFromSquare />在浏览器打开
          </Button>
          <Button variant="secondary" onPress={() => copyText(session.url).then(() => toast.success('链接已复制'), () => toast.danger('复制失败，请手动选择复制'))}>
            <Copy />复制链接
          </Button>
        </div>
        {poll.error ? (
          <Alert status="danger">
            <Alert.Indicator />
            <Alert.Content>
              <Alert.Title>授权失败</Alert.Title>
              <Alert.Description>{errorMessage(poll.error)}（关闭后重新添加）</Alert.Description>
            </Alert.Content>
          </Alert>
        ) : (
          <p className="flex items-center gap-2 text-sm text-muted"><Spinner size="sm" />等待授权完成，自动检测中…</p>
        )}
      </div>
    )
  }

  return (
    <div className="flex flex-col gap-4">
      <RadioGroup value={realm} onChange={setRealm} orientation="horizontal">
        <Label>版本</Label>
        <Radio value="cn"><Radio.Content><Radio.Control><Radio.Indicator /></Radio.Control>国内版（CN）</Radio.Content></Radio>
        <Radio value="global"><Radio.Content><Radio.Control><Radio.Indicator /></Radio.Control>国际版（Global）</Radio.Content></Radio>
        <Description>国际版登录后，网关自动完成注册地区、激活与试用额度领取，全程无需手动操作。</Description>
      </RadioGroup>
      {startError && <p className="text-sm text-danger">{startError}</p>}
      <div>
        <BusyButton variant="primary" busy={starting} onPress={() => void start()}>获取授权链接</BusyButton>
      </div>
    </div>
  )
}

function ImportPanel() {
  const qc = useQueryClient()
  const input = useRef<HTMLInputElement>(null)
  const [busy, setBusy] = useState(false)
  const [result, setResult] = useState<ImportResp | null>(null)
  const [error, setError] = useState('')

  const onFile = async (file: File | undefined) => {
    if (!file) return
    setBusy(true)
    setResult(null)
    setError('')
    const fd = new FormData()
    fd.append('file', file)
    try {
      setResult(await upload<ImportResp>('import/cockpit', fd))
      void qc.invalidateQueries({ queryKey: qk.overview })
    } catch (e) {
      setError('导入失败：' + errorMessage(e))
    } finally {
      setBusy(false)
      if (input.current) input.current.value = ''
    }
  }

  const errors = result?.errors ?? []
  return (
    <div className="flex flex-col gap-3">
      <p className="text-sm text-muted">选择 cockpit tools 导出的 JSON 文件，批量导入账号。文件需为账号数组格式。</p>
      <input ref={input} type="file" accept=".json,application/json" hidden onChange={(e) => void onFile(e.target.files?.[0])} />
      <div>
        <BusyButton variant="secondary" busy={busy} onPress={() => input.current?.click()}>选择 JSON 文件</BusyButton>
      </div>
      {error && <p className="text-sm text-danger">{error}</p>}
      {result && (
        <Alert status={errors.length ? 'warning' : 'success'}>
          <Alert.Indicator />
          <Alert.Content>
            <Alert.Title>
              导入完成：成功 {result.imported} 个{result.skipped ? '，跳过 ' + result.skipped + ' 个' : ''}
              {errors.length ? '，' + errors.length + ' 个出错' : ''}
            </Alert.Title>
            {errors.length > 0 && (
              <Alert.Description>
                <ul className="mt-1 list-disc space-y-0.5 pl-4 text-xs">
                  {errors.map((e, i) => <li key={i} className="break-all">{e}</li>)}
                </ul>
              </Alert.Description>
            )}
          </Alert.Content>
        </Alert>
      )}
    </div>
  )
}
