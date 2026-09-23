<script>
  import { onMount } from 'svelte';
  import { api } from '../lib/api.js';
  import { ui, toast, authFailed } from '../lib/stores.svelte.js';

  const SCHOOL_META = [
    ['share_invite', '分享'],
    ['desktop_chat_1_time', '桌面'],
    ['chat_3_times', '对话×3'],
    ['expert_use', '专家'],
    ['task_student_verify', '认证']
  ];
  const SCHOOL_TITLES = {
    share_invite: '分享活动 +100c',
    desktop_chat_1_time: '桌面端体验 +100c（单次）',
    chat_3_times: '和 AI 对话 3 次 +50c',
    expert_use: '召唤开学季专家 +50c',
    task_student_verify: '学生认证 +100c（需真实认证，不做）'
  };
  const ST_WORDS = { done: '完成', running: '执行中', error: '失败', skipped: '跳过', pending: '排队', scan: '待执行' };

  let school = $state([]);
  let schoolState = $state('loading'); // loading | ready | error
  let schoolMsg = $state('');
  let schoolSummary = $state('');
  let groups = $state([]);
  let progress = $state(null);
  let emptyTitle = $state('还没有扫描过');
  let emptyDesc = $state('扫描所有账号的成长任务与开学季待办，把没做的排成一列，一键执行。');
  let conc = $state('1');
  let scanning = $state(false);
  let running = $state(false);

  let queueTimer = $state(null);
  let lastQueueSeq = $state(0);
  const growthTitles = {};

  function stask(t) {
    if (!t) return { cls: 'todo', mark: '·', text: '—' };
    if (t.status === 'claimed') return { cls: 'ok', mark: '✓', text: '已领' };
    if (t.status === 'completed') return { cls: 'warn', mark: '◆', text: '可领' };
    if (t.status === 'in_progress') {
      const fr = t.target_count ? t.progress + '/' + t.target_count : '';
      return { cls: 'warn', mark: '◐', text: '', fr };
    }
    return { cls: 'todo', mark: '○', text: '未做' };
  }

  async function loadSchool(quiet = false) {
    if (ui.route !== 'taskscenter' && !quiet) return;
    if (!quiet) schoolState = 'loading';
    try {
      const d = await api('school/status');
      const arr = d.accounts || [];
      if (!arr.length) {
        schoolState = 'error';
        schoolMsg = '暂无可用账号';
        school = [];
        return;
      }
      school = arr;
      const doneCount = arr.filter((v) => {
        const by = {};
        (v.tasks || []).forEach((t) => (by[t.task_code] = t));
        return SCHOOL_META.filter(([code]) => code !== 'task_student_verify').every(
          ([code]) => by[code] && by[code].status === 'claimed'
        );
      }).length;
      schoolSummary = doneCount === arr.length ? '今日全部完成 🎉' : doneCount + '/' + arr.length + ' 个账号今日全部完成';
      schoolState = 'ready';
    } catch (e) {
      if (e.unauthorized) authFailed();
      else {
        schoolState = 'error';
        schoolMsg = e.message;
      }
    }
  }

  async function runAll() {
    if (!confirm('将对全部账号执行开学季闭环（分享/桌面/对话/专家 + 抽奖），约 1-2 分钟。确认继续？')) return;
    try {
      await api('school/run_all', { method: 'POST' });
      toast('开学季闭环已开始，结果看任务日志', 'ok');
      setTimeout(() => loadSchool(true), 15000);
    } catch (e) {
      if (e.unauthorized) authFailed();
      else toast(e.message, 'err');
    }
  }

  function groupItems(d) {
    const out = [];
    for (const a of d.accounts || []) {
      const rows = [];
      for (const t of a.growth || []) {
        growthTitles[t.task_code] = t.title || t.task_code;
        rows.push({ kind: 'growth', code: t.task_code, prog: t.target ? t.current + '/' + t.target : '—', status: 'scan' });
      }
      for (const t of a.school || []) {
        if (t.task_code === 'task_student_verify') continue;
        rows.push({
          kind: 'school',
          code: t.task_code,
          prog: t.target_count ? t.progress + '/' + t.target_count : '—',
          status: 'scan'
        });
      }
      if (rows.length) out.push({ uid: a.uid, nick: a.nickname, rows });
    }
    return out;
  }

  function groupsFromQueue(items) {
    const by = new Map();
    for (const it of items) {
      if (!by.has(it.uid)) by.set(it.uid, { uid: it.uid, nick: it.nickname, rows: [] });
      by.get(it.uid).rows.push({
        kind: it.kind,
        code: it.code,
        prog: it.kind === 'school' ? '—' : '',
        status: it.status,
        message: it.message
      });
    }
    return Array.from(by.values());
  }

  async function scanAll() {
    scanning = true;
    try {
      const d = await api('tasks/scan_all', { method: 'POST' });
      groups = groupItems(d);
      progress = null;
      if (!groups.length) {
        emptyTitle = '没有待办任务 🎉';
        emptyDesc = '全部账号的成长任务与开学季活动都已完成，明日再来。';
      }
    } catch (e) {
      if (e.unauthorized) authFailed();
      else toast(e.message, 'err');
    } finally {
      scanning = false;
    }
  }

  async function runQueue() {
    const c = Number(conc) || 1;
    if (!confirm(`扫描全部账号待办并排队执行（账号并发 ${c}，账号内串行）。\n含真实对话的任务耗时较长，确认继续？`)) return;
    running = true;
    try {
      const r = await api('tasks/run_queue', { method: 'POST', body: { concurrency: c } });
      if (!r.started) {
        toast(r.message || '没有待办任务', 'ok');
        return;
      }
      lastQueueSeq = r.seq || 0;
      toast('队列已启动：' + r.total + ' 项（并发 ' + c + '）', 'ok');
      startQueuePolling();
    } catch (e) {
      if (e.unauthorized) authFailed();
      else toast(e.message, 'err');
    } finally {
      running = false;
    }
  }

  async function pollQueueOnce() {
    try {
      const q = await api('tasks/queue');
      if (!q.started) return;
      if (lastQueueSeq && q.seq !== lastQueueSeq) return;
      groups = groupsFromQueue(q.items || []);
      progress = q;
    } catch {
      /* 静默 */
    }
  }

  function startQueuePolling() {
    if (queueTimer) clearInterval(queueTimer);
    queueTimer = setInterval(async () => {
      await pollQueueOnce();
      try {
        const q = await api('tasks/queue');
        if (!q.running) {
          clearInterval(queueTimer);
          queueTimer = null;
          toast('任务队列执行结束', 'ok');
          loadSchool(true);
        }
      } catch {
        /* 忽略 */
      }
    }, 3000);
  }

  function dotCls(st) {
    return st === 'scan'
      ? 'wait'
      : st === 'running'
        ? 'run'
        : st === 'error'
          ? 'err'
          : st === 'skipped'
            ? 'skip'
            : st === 'done'
              ? 'done'
              : 'wait';
  }

  let doneCount = $derived(
    progress && progress.items
      ? progress.items.filter((it) => it.status === 'done' || it.status === 'error' || it.status === 'skipped').length
      : 0
  );
  let totalCount = $derived(progress && progress.items ? progress.items.length : 0);

  onMount(() => {
    loadSchool(false);
    pollQueueOnce();
    const h = () => {
      if (ui.route === 'taskscenter') pollQueueOnce();
    };
    addEventListener('wb:refresh', h);
    const t = setInterval(() => {
      if (ui.route === 'taskscenter' && !queueTimer) pollQueueOnce();
    }, 5000);
    return () => {
      removeEventListener('wb:refresh', h);
      clearInterval(t);
      if (queueTimer) clearInterval(queueTimer);
    };
  });
</script>

<div class="box">
  <header>
    <h3>开学季 <span class="hint">09-13 ~ 09-24 · 每日重置</span></h3>
    <span class="grow"></span>
    <span class="note">{schoolSummary}</span>
    <button class="btn xs" onclick={() => (ui.vouchersOpen = true)}>查询券码</button>
    <button class="btn xs" onclick={() => loadSchool(false)}>刷新</button>
    <button class="btn xs primary" onclick={runAll}>全部账号执行闭环</button>
  </header>
  {#if schoolState === 'loading'}
    <div class="state px-4 py-3"><span class="dots">加载中</span></div>
  {:else if schoolState === 'error'}
    <div class="state err px-4 py-3">{schoolMsg}</div>
  {:else}
    <div class="shead">
      <div class="who">账号</div>
      <div class="stasks">
        {#each SCHOOL_META as [, name] (name)}<span>{name}</span>{/each}
      </div>
      <div class="luck">剩余抽奖</div>
    </div>
    {#each school as v (v.uid)}
      {@const by = Object.fromEntries((v.tasks || []).map((t) => [t.task_code, t]))}
      <div class="srow">
        <div class="who">
          <div class="nm" title={v.nickname || ''}>{v.nickname || '未命名'}</div>
          <div class="id">{v.uid}</div>
        </div>
        <div class="stasks">
          {#each SCHOOL_META as [code] (code)}
            {@const tt = by[code]}
            <span title={SCHOOL_TITLES[code] || code}>
              {#if code === 'task_student_verify'}
                <span class="stask todo"><span class="mark">—</span>不做</span>
              {:else}
                {@const s = stask(tt)}
                <span class="stask {s.cls}">
                  <span class="mark">{s.mark}</span>{s.text}{#if s.fr}<span class="fr">{s.fr}</span>{/if}
                </span>
              {/if}
            </span>
          {/each}
        </div>
        <div class="luck" title="剩余抽奖次数">
          <svg viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.4">
            <path
              d="M3.2 5.2 5 1.8l3 2.4 3-2.4 1.8 3.4-1.4 2.6 1.4 2.6-3.4 2.2H6l-3.4-2.2 1.4-2.6z"
              opacity=".9"
            /><circle cx="8" cy="9" r="1.1" fill="currentColor" stroke="none" />
          </svg>
          {v.chances == null ? '—' : v.chances}
        </div>
        {#if v.error}<div class="err">{v.error}</div>{/if}
      </div>
    {/each}
  {/if}
</div>

<div class="box">
  <header>
    <h3>成长任务队列 <span class="hint">{groups.length ? groups.reduce((a, g) => a + g.rows.length, 0) + ' 项' : ''}</span></h3>
    <span class="grow"></span>
    <label class="inline-flex items-center gap-1.5 text-[12px] text-ink3">
      并发
      <select class="fld-input" style="width: auto; padding: 3px 8px; font-size: 12px;" bind:value={conc}>
        <option value="1">1</option>
        <option value="2">2</option>
        <option value="3">3</option>
      </select>
    </label>
    <button class="btn xs" onclick={scanAll} disabled={scanning}>{scanning ? '扫描中…' : '扫描待办'}</button>
    <button class="btn xs primary" onclick={runQueue} disabled={running}>{running ? '启动中…' : '执行全部待办'}</button>
  </header>
  {#if progress && progress.items}
    <div class="qprog">
      <div class="qbar"><i style="width: {totalCount ? Math.round((doneCount / totalCount) * 100) : 0}%;"></i></div>
      <span class="note">{progress.running ? '执行中 ' : '已结束 '}{doneCount} / {totalCount}</span>
    </div>
  {/if}
  {#if !groups.length}
    <div class="tc-empty flex flex-col items-center gap-1.5 px-4 py-[42px] text-ink3">
      <svg viewBox="0 0 48 48" fill="none" stroke="currentColor" stroke-width="2" class="mb-1.5 h-[42px] w-[42px] opacity-55" aria-hidden="true">
        <rect x="8" y="6" width="24" height="36" rx="3" />
        <path d="M14 16h12M14 24h12M14 32h7" opacity=".45" />
        <circle cx="35" cy="33" r="8" /><path d="m40 38 5 5" />
      </svg>
      <div class="t font-medium text-ink2">{emptyTitle}</div>
      <div class="d max-w-[34em] text-center text-[12.5px]">{emptyDesc}</div>
    </div>
  {:else}
    <div>
      {#each groups as g (g.uid)}
        <div class="qgroup">
          <header><span class="nm">{g.nick || String(g.uid).slice(0, 12)}</span><span class="cnt">{g.rows.length} 项待办</span></header>
          {#each g.rows as it, i (`${it.kind}:${it.code}:${i}`)}
            {@const title = it.kind === 'school' ? '开学季闭环' : growthTitles[it.code] || it.code}
            <div class="qrow" title={it.message || ''}>
              <span class="code">{it.code}</span>
              <span class="name">
                <span class="t">{title}</span>
                {#if it.kind === 'school'}<span class="tag mute">开学季</span>{/if}
              </span>
              <span class="prog">{it.prog || ''}</span>
              <span class="st"><span class="qdot {dotCls(it.status)}"></span>{it.status === 'scan' ? '待执行' : ST_WORDS[it.status] || it.status}</span>
              <span class="msg">{it.message || ''}</span>
            </div>
          {/each}
        </div>
      {/each}
    </div>
  {/if}
</div>
