<script>
  import { api } from '../lib/api.js';
  import { ui, toast, authFailed } from '../lib/stores.svelte.js';
  import Modal from './Modal.svelte';

  // 可自动完成的任务（与后端 autoActions 表一致）：判据为行为事件、可经网关复现。
  // 其余任务需在官方客户端内交互，面板只展示指引（行 title 提示）。
  const AUTO_TASKS = {
    chat_5: '上报 5 条对话活跃事件（自动补足差额）',
    first_buddy: '上报解锁 → 同意协议 → 领取第一只 Buddy',
    'Model_chat_GLM5.2': '接受任务 → glm-5.2 真实对话一次 → 对齐模型上报',
    RichMeow_Chat: '桌面指纹事件链上报（已验证可点亮）',
    Buddy_App: '上报「进入 Buddy 应用」事件链（已验证可点亮）',
    Buddy_App_QQ: '上报「进入企鹅教师助手」事件链（已验证可点亮）',
    automation_1: '上报「定时任务创建」事件（已验证可点亮）',
    Library_read: '上报「读资料库介绍」事件（已验证可点亮）',
    template_5: '上报「使用模板创建任务」事件组 ×5（三账号实测点亮）',
    playbook_prompt: '上报「灵感案例做同款发送 Prompt」事件组（三账号实测点亮）',
    create_canvas: '上报「设计创意画布创建」事件组（三账号实测点亮，+300 分）',
    expert_5: '真实专家召唤+使用链 ×5（专家市场+真实 chat，三账号实测点亮）',
    Expert_team_use_3: '真实专家团召唤+使用链 ×3（三账号实测点亮）',
    Hp_Appearance: '设置主题 API + 皮肤生效事件（两账号实测点亮）',
    black_cat: '夜猫子：23:00–08:00 窗口内 glm-5.2 对话补足（窗口外提示等 23 点排程）',
    Expert_lighthouse: '真实轻量云专家召唤+使用链（真实对话 requestId，两账号实测点亮）',
    skill_1: '真实对话 + skill_info 技能加载事件（实测点亮）',
    school_season: '校园日（小程序口径）：accept → mini 对话+activityId 上报 → 领奖（+100c+5e）',
    Sequential_Tasks_1: '小程序首对话（小程序口径）：accept → mini 对话上报 → 领奖（+100c+5e）'
  };

  let tasks = $state([]);
  let state = $state('loading'); // loading | ready | empty | error
  let stateMsg = $state('');
  let busy = $state(false);

  async function load() {
    const uid = ui.taskUid;
    if (!uid) return;
    state = 'loading';
    tasks = [];
    try {
      const d = await api('accounts/' + encodeURIComponent(uid) + '/tasks');
      const list = d.tasks || [];
      if (!list.length) {
        state = 'empty';
        return;
      }
      // 有进度或可领取的排前面，已领取沉底——一眼看到"现在该做什么"。
      list.sort(
        (a, b) => a.claimed - b.claimed || b.claimable - a.claimable || String(a.task_code).localeCompare(String(b.task_code))
      );
      tasks = list;
      state = 'ready';
    } catch (e) {
      if (e.unauthorized) authFailed();
      state = 'error';
      stateMsg = e.message;
    }
  }

  function rewardOf(t) {
    const parts = [];
    if (t.credit) parts.push('+' + t.credit + ' 分');
    if (t.energy) parts.push('+' + t.energy + ' 能');
    if (t.reward_buddy) parts.push('Buddy');
    return parts.length ? parts.join(' ') : '—';
  }

  function tipOf(t) {
    return [t.title, t.task_desc || t.description, t.jump_url ? '跳转：' + t.jump_url : ''].filter(Boolean).join('\n');
  }

  async function act(kind, code, btn) {
    const uid = ui.taskUid;
    if (!uid || busy) return;
    busy = true;
    try {
      if (kind === 'auto') {
        const r = await api('accounts/' + encodeURIComponent(uid) + '/tasks/auto', {
          method: 'POST',
          body: { task_code: code }
        });
        if (r.skipped) {
          toast(r.message || '已跳过', 'ok');
        } else {
          const advanced = r.progress_before !== r.progress_after;
          let m = r.message || '已执行';
          if (r.progress_after) m += `（进度 ${r.progress_before} → ${r.progress_after}）`;
          if (r.claimed) m += '，奖励已自动到账';
          else if (r.claimable) m += r.claim_error ? '，可点「领取」重试' : '';
          else if (r.attempt && !advanced) m += '；进度未动，该任务可能需要官方客户端';
          toast(m, r.claimed || advanced ? 'ok' : 'err');
        }
        dispatchEvent(new CustomEvent('wb:refresh'));
      } else {
        const path = 'accounts/' + encodeURIComponent(uid) + '/tasks/' + (kind === 'claim' ? 'claim' : 'accept');
        const body = kind === 'claim' ? { task_code: code } : { task_codes: [code] };
        await api(path, { method: 'POST', body });
        toast(kind === 'claim' ? '已领取奖励' : '已接受任务', 'ok');
        if (kind === 'claim') dispatchEvent(new CustomEvent('wb:refresh'));
      }
    } catch (e) {
      if (e.unauthorized) authFailed();
      else toast(e.message, 'err');
    } finally {
      busy = false;
      load();
    }
  }

  async function acceptAll() {
    const uid = ui.taskUid;
    if (!uid || busy) return;
    busy = true;
    try {
      const r = await api('accounts/' + encodeURIComponent(uid) + '/tasks/accept_all', { method: 'POST' });
      const n = r.accepted || 0;
      if (r.failed && r.failed.length) toast(`已接受 ${n} 个，${r.failed.length} 个被上游拒绝（可重试）`, 'err');
      else toast(n ? `已接受 ${n} 个任务` : r.message || '所有任务均已接受', 'ok');
    } catch (e) {
      if (e.unauthorized) authFailed();
      else toast(e.message, 'err');
    } finally {
      busy = false;
      load();
    }
  }

  async function autoAll() {
    const uid = ui.taskUid;
    if (!uid || busy) return;
    if (!confirm('将依次执行：补报对话事件、领取 Buddy、glm-5.2 对话、尝试上报。\n过程约 1-2 分钟（含真实对话），确认继续？')) return;
    busy = true;
    try {
      const r = await api('accounts/' + encodeURIComponent(uid) + '/tasks/auto_all', { method: 'POST' });
      const okN = (r.results || []).filter((x) => x.status === 'done').length;
      const skipN = (r.results || []).filter((x) => x.status === 'skipped').length;
      const errN = (r.results || []).filter((x) => x.status === 'error').length;
      toast(`执行完成：成功 ${okN} 项，跳过 ${skipN} 项${errN ? '，失败 ' + errN + ' 项' : ''}`, errN ? 'err' : 'ok');
    } catch (e) {
      if (e.unauthorized) authFailed();
      else toast(e.message, 'err');
    } finally {
      busy = false;
      load();
    }
  }

  $effect(() => {
    if (ui.taskUid) load();
    else {
      tasks = [];
      state = 'loading';
    }
  });
</script>

<Modal
  open={ui.taskUid !== null}
  title="积分任务"
  hint={ui.taskUid ? String(ui.taskUid).slice(0, 16) : ''}
  wide={true}
  onclose={() => {
    ui.taskUid = null;
  }}
>
  {#if state === 'loading'}
    <div class="state"><span class="dots">查询中</span></div>
  {:else if state === 'empty'}
    <div class="state">该账号暂无任务</div>
  {:else if state === 'error'}
    <div class="state err">{stateMsg}</div>
  {:else}
    <div class="tbl-wrap">
      <table class="acc md">
        <thead>
          <tr>
            <th>任务</th>
            <th>进度</th>
            <th>奖励</th>
            <th>状态</th>
            <th class="c-acts"></th>
          </tr>
        </thead>
        <tbody>
          {#each tasks as t (t.task_code)}
            {@const cur = t.current ?? 0}
            {@const tgt = t.target ?? 0}
            {@const prog = tgt ? cur + ' / ' + tgt : tgt === 0 && cur > 0 ? String(cur) : '—'}
            <tr title={tipOf(t)}>
              <td class="who">
                <div class="nm">{t.title || t.task_code}</div>
                <div class="id">{t.task_code}{t.tag ? ' · ' + t.tag : ''}</div>
              </td>
              <td class="num">{prog}</td>
              <td class="num">{rewardOf(t)}</td>
              <td>
                {#if t.claimed}<span class="tag ok">已领取</span>
                {:else if t.claimable}<span class="tag warn">可领取</span>
                {:else if t.locked}<span class="tag mute">未解锁</span>
                {:else if t.accept_status === 'accepted'}<span class="tag mute">进行中</span>
                {:else}<span class="tag mute">未接受</span>{/if}
              </td>
              <td class="acts">
                {#if !t.claimed && !t.locked}
                  {#if t.claimable}
                    <button class="btn xs primary" disabled={busy} onclick={(e) => act('claim', t.task_code, e.currentTarget)}>领取</button>
                  {:else if AUTO_TASKS[t.task_code]}
                    <button
                      class="btn xs primary"
                      disabled={busy}
                      title={AUTO_TASKS[t.task_code]}
                      onclick={(e) => act('auto', t.task_code, e.currentTarget)}>一键完成</button
                    >
                  {:else if t.accept_status !== 'accepted'}
                    <button class="btn xs" disabled={busy} onclick={(e) => act('accept', t.task_code, e.currentTarget)}>接受</button>
                  {/if}
                {/if}
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
  {/if}
  {#snippet footer()}
    <span class="grow"></span>
    <button class="btn" disabled={busy} onclick={acceptAll}>全部接受</button>
    <button class="btn primary" disabled={busy} onclick={autoAll}>一键完成可自动任务</button>
    <button class="btn" onclick={load}>重新查询</button>
    <button
      class="btn"
      onclick={() => {
        ui.taskUid = null;
      }}>关闭</button
    >
  {/snippet}
</Modal>
