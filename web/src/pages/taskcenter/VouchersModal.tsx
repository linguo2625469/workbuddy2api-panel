// 开学季券码（活动 9/24 已结束，历史券码仍可查）：抽奖抽中的第三方券（KFC / 瑞幸 / 酷狗等），
// 券码到对应 app / 小程序兑换。二维码由 uqr 在本地生成（面板 CSP 只允许同源，不能用外部二维码服务）。
import { useState } from 'react'
import { Copy, QrCode } from '@gravity-ui/icons'
import { Button, Card, Chip, Modal, toast } from '@heroui/react'
import { encode } from 'uqr'
import { copyText } from '../../lib/clipboard'
import { useVouchers } from '../../lib/queries'
import type { Voucher } from '../../lib/types'
import { Empty, Loaded } from '../../components/Feedback'

export function VouchersModal({ open, onClose }: { open: boolean; onClose: () => void }) {
  const query = useVouchers(open)
  return (
    <Modal.Backdrop isOpen={open} onOpenChange={(v) => { if (!v) onClose() }}>
      <Modal.Container size="md" scroll="inside" placement="auto">
        <Modal.Dialog>
          <Modal.CloseTrigger />
          <Modal.Header>
            <Modal.Heading>开学季 · 我的券码</Modal.Heading>
            <p className="mt-1.5 text-sm text-muted">抽奖抽中的第三方券（KFC / 瑞幸 / 酷狗等），券码到对应 app / 小程序兑换。</p>
          </Modal.Header>
          <Modal.Body className="flex flex-col gap-3">
            <Loaded query={query} rows={3}>
              {(d) => {
                const arr = d.accounts || []
                const ok = arr.filter((a) => !a.error)
                const withV = ok.filter((a) => (a.vouchers || []).length)
                const errs = arr.filter((a) => a.error)
                return (
                  <>
                    {withV.length === 0 && <Empty title="还没有抽到券" />}
                    {withV.map((a) => (
                      <section key={a.uid} className="flex flex-col gap-2">
                        <h3 className="text-sm"><span className="font-semibold">{a.nickname || a.uid}</span> <span className="text-muted">{a.vouchers!.length} 张</span></h3>
                        {a.vouchers!.map((v, i) => <VoucherCard key={v.code || i} v={v} />)}
                      </section>
                    ))}
                    {errs.length > 0 && (
                      <p className="text-sm text-warning">
                        查询失败：{errs.map((a) => (a.nickname || a.uid.slice(0, 8)) + '（' + a.error + '）').join('、')}
                      </p>
                    )}
                    {withV.length > 0 && (
                      <p className="text-xs text-muted">
                        {withV.reduce((n, a) => n + a.vouchers!.length, 0)} 张券 · {ok.length - withV.length} 个账号未抽中
                      </p>
                    )}
                  </>
                )
              }}
            </Loaded>
          </Modal.Body>
          <Modal.Footer>
            <Button variant="secondary" isPending={query.isFetching} onPress={() => void query.refetch()}>刷新</Button>
            <Button variant="primary" onPress={onClose}>关闭</Button>
          </Modal.Footer>
        </Modal.Dialog>
      </Modal.Container>
    </Modal.Backdrop>
  )
}

function VoucherCard({ v }: { v: Voucher }) {
  const [qr, setQr] = useState(false)
  const expired = !!v.valid_to && new Date(v.valid_to) < new Date()
  const copy = () => copyText(v.code).then(() => toast.success('券码已复制'), () => toast.danger('复制失败，请手动选择券码'))
  return (
    <Card variant="secondary" className={`gap-2 p-4 ${expired ? 'opacity-70' : ''}`}>
      <div className="flex items-center justify-between gap-2">
        <span className="font-medium">{v.prize_name || v.sku_code || '券'}</span>
        {expired ? <Chip size="sm" variant="soft" color="danger">已过期</Chip> : <Chip size="sm" variant="soft" color="success">可使用</Chip>}
      </div>
      <p className="text-xs text-muted">
        {v.valid_to ? '有效期至 ' + v.valid_to : '长期有效'}
        {v.granted_at ? ' · ' + v.granted_at.slice(0, 10) + ' 抽中' : ''}
      </p>
      <div className="flex flex-wrap items-center gap-2">
        <code className="select-all rounded-lg bg-surface px-3 py-1.5 font-mono text-base font-semibold tracking-wider">{v.code || '-'}</code>
        <div className="ml-auto flex gap-1">
          {v.code && <Button size="sm" variant="ghost" onPress={() => setQr(!qr)}><QrCode />{qr ? '收起' : '二维码'}</Button>}
          <Button size="sm" variant="ghost" isDisabled={!v.code} onPress={() => void copy()}><Copy />复制</Button>
        </div>
      </div>
      {qr && v.code && <div className="flex justify-center pt-2"><QrSvg text={v.code} /></div>}
    </Card>
  )
}

/** 券码二维码：白底黑块，四格留白（扫码枪要求） */
function QrSvg({ text }: { text: string }) {
  const { data } = encode(text, { border: 4 })
  const n = data.length
  let d = ''
  data.forEach((row, y) => row.forEach((on, x) => { if (on) d += `M${x} ${y}h1v1h-1z` }))
  return (
    <svg viewBox={`0 0 ${n} ${n}`} width={160} height={160} shapeRendering="crispEdges" role="img" aria-label={'券码 ' + text + ' 的二维码'} className="rounded-md bg-white">
      <path d={d} fill="#000" />
    </svg>
  )
}
