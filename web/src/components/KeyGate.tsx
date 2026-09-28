import { useEffect, useState } from 'react'
import { FieldError, Form, Input, Label, Modal, TextField } from '@heroui/react'
import { useQueryClient } from '@tanstack/react-query'
import { AUTH_EVENT, api, setKey } from '../lib/api'
import { BusyButton } from './Feedback'

/** 网关开了 api_key 鉴权时，任一接口返回 401 就弹出这个框；输对之前不能关闭 */
export function KeyGate() {
  const qc = useQueryClient()
  const [open, setOpen] = useState(false)
  const [value, setValue] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    const on = () => setOpen(true)
    window.addEventListener(AUTH_EVENT, on)
    return () => window.removeEventListener(AUTH_EVENT, on)
  }, [])

  const submit = async () => {
    const k = value.trim()
    if (!k) return
    setKey(k)
    setBusy(true)
    try {
      await api('overview')
      setError('')
      setOpen(false)
      await qc.invalidateQueries()
    } catch {
      setError('密钥不正确，请重试。')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Modal.Backdrop isOpen={open} isDismissable={false} isKeyboardDismissDisabled>
      <Modal.Container size="sm">
        <Modal.Dialog>
          <Modal.Header>
            <Modal.Heading>需要访问密钥</Modal.Heading>
            <p className="mt-1.5 text-sm text-muted">该网关已启用 api_key 鉴权，请输入 config.json 中的密钥。</p>
          </Modal.Header>
          <Form className="contents" onSubmit={(e) => { e.preventDefault(); void submit() }}>
            <Modal.Body>
              <TextField autoFocus isInvalid={!!error} value={value} onChange={setValue} type="password" name="api_key">
                <Label>api_key</Label>
                <Input autoComplete="current-password" />
                <FieldError>{error}</FieldError>
              </TextField>
            </Modal.Body>
            <Modal.Footer>
              <BusyButton type="submit" busy={busy} isDisabled={!value.trim()}>进入</BusyButton>
            </Modal.Footer>
          </Form>
        </Modal.Dialog>
      </Modal.Container>
    </Modal.Backdrop>
  )
}
