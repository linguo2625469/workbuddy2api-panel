// 运行日志：最近 500 行（网关内存环形缓冲），按频道 / 级别 / 关键字筛选。
// 停在底部时每 5 秒跟随新日志；往上翻看旧日志时冻结画面（日志环满了会从顶部淘汰旧行，
// 不冻结的话翻看中的内容会整体上移），滚回底部或点「回到最新」再继续跟随。
import { useLayoutEffect, useMemo, useRef, useState } from 'react'
import { Button, Chip, SearchField, ToggleButton, ToggleButtonGroup } from '@heroui/react'
import { useNavigate, useSearch } from '@tanstack/react-router'
import { CHANNEL_LABEL, LOG_CHANNELS, filterLogs, logLevel, type LevelFilter, type LogChannel } from '../../lib/logs'
import { useLogs } from '../../lib/queries'
import type { LogEntry } from '../../lib/types'
import { Empty, Loaded } from '../../components/Feedback'
import { Panel } from '../../components/Panel'

const CH_COLOR: Record<string, 'accent' | 'success' | 'default'> = { task: 'accent', chat: 'success', sys: 'default' }
const LEVEL_TEXT = { error: 'text-danger', warn: 'text-warning', info: '' }

/** 冻结后又来了多少行（日志环满 500 行后总数不变，所以按冻结时最后一行的位置算） */
function newerThan(live: LogEntry[], frozen: LogEntry[]): number {
  const last = frozen[frozen.length - 1]
  if (!last) return live.length
  for (let i = live.length - 1; i >= 0; i--) {
    if (live[i].ts === last.ts && live[i].text === last.text) return live.length - 1 - i
  }
  return live.length
}

export function LogsPage() {
  const { ch = 'all' } = useSearch({ from: '/logs' })
  const navigate = useNavigate({ from: '/logs' })
  const [level, setLevel] = useState<LevelFilter>('all')
  const [q, setQ] = useState('')
  const [following, setFollowing] = useState(true)
  const [frozen, setFrozen] = useState<LogEntry[] | null>(null)
  const query = useLogs(true)
  const box = useRef<HTMLDivElement>(null)

  const live = useMemo(() => query.data?.entries || [], [query.data])
  const entries = following ? live : (frozen ?? live)
  const shown = useMemo(() => filterLogs(entries, ch, level, q), [entries, ch, level, q])
  const newer = following || !frozen ? 0 : newerThan(live, frozen)
  const counts = useMemo(() => {
    const c: Record<string, number> = {}
    for (const e of live) c[e.ch] = (c[e.ch] || 0) + 1
    return c
  }, [live])

  // 跟随时新内容渲染后贴底
  useLayoutEffect(() => {
    if (following && box.current) box.current.scrollTop = box.current.scrollHeight
  }, [shown, following])

  const onScroll = () => {
    const el = box.current
    if (!el) return
    const atEnd = el.scrollTop + el.clientHeight >= el.scrollHeight - 24
    if (atEnd && !following) {
      setFollowing(true)
      setFrozen(null)
    } else if (!atEnd && following) {
      setFollowing(false)
      setFrozen(live)
    }
  }

  const backToLatest = () => {
    setFrozen(null)
    setFollowing(true)
  }

  return (
    <Panel
      title="运行日志"
      desc={ch === 'all'
        ? `最近 ${live.length} 行 · 任务 ${counts.task || 0} · 对话 ${counts.chat || 0} · 系统 ${counts.sys || 0}`
        : `${CHANNEL_LABEL[ch]} ${counts[ch] || 0} 行`}
      contentClassName="flex flex-col gap-3"
    >
      <div className="flex flex-wrap items-center gap-3">
        <ToggleButtonGroup aria-label="频道" size="sm" selectionMode="single" disallowEmptySelection
          selectedKeys={[ch]} onSelectionChange={(k) => void navigate({ search: { ch: [...k][0] as LogChannel }, replace: true })}>
          {LOG_CHANNELS.map((c, i) => (
            <ToggleButton key={c} id={c}>{i > 0 && <ToggleButtonGroup.Separator />}{CHANNEL_LABEL[c]}</ToggleButton>
          ))}
        </ToggleButtonGroup>
        <ToggleButtonGroup aria-label="级别" size="sm" selectionMode="single" disallowEmptySelection
          selectedKeys={[level]} onSelectionChange={(k) => setLevel([...k][0] as LevelFilter)}>
          <ToggleButton id="all">全部级别</ToggleButton>
          <ToggleButton id="warn"><ToggleButtonGroup.Separator />警告及错误</ToggleButton>
          <ToggleButton id="error"><ToggleButtonGroup.Separator />仅错误</ToggleButton>
        </ToggleButtonGroup>
        <SearchField aria-label="搜索日志" value={q} onChange={setQ} className="w-full sm:w-64">
          <SearchField.Group>
            <SearchField.SearchIcon />
            <SearchField.Input placeholder="搜索日志内容" />
            <SearchField.ClearButton />
          </SearchField.Group>
        </SearchField>
        {!following && (
          <Button size="sm" variant="primary" className="sm:ml-auto" onPress={backToLatest}>
            回到最新{newer > 0 ? `（${newer} 行新日志）` : ''}
          </Button>
        )}
      </div>
      <Loaded query={query} rows={8}>
        {() => (
          <div
            ref={box}
            onScroll={onScroll}
            className="h-[60dvh] min-h-72 overflow-auto rounded-xl bg-surface-secondary p-3 font-mono text-xs leading-6"
          >
            {shown.length === 0 ? (
              <Empty title={live.length ? '没有符合条件的日志' : '暂无日志'} />
            ) : (
              shown.map((e, i) => {
                const t = e.ts ? new Date(e.ts).toLocaleTimeString('zh-CN', { hour12: false }) : ''
                return (
                  <div key={i} className={`whitespace-pre-wrap break-all ${LEVEL_TEXT[logLevel(e.text)]}`}>
                    {ch === 'all' && <Chip size="sm" variant="soft" color={CH_COLOR[e.ch] ?? 'default'} className="mr-2 align-middle">{CHANNEL_LABEL[e.ch] ?? e.ch}</Chip>}
                    <span className="text-muted">{t}</span> {e.text}
                  </div>
                )
              })
            )}
          </div>
        )}
      </Loaded>
    </Panel>
  )
}
