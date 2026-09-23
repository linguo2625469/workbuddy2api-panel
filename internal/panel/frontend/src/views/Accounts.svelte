<script>
  import { api } from '../lib/api.js';
  import { ui, overview, toast, authFailed } from '../lib/stores.svelte.js';
  import { ago, dur, shortUid } from '../lib/format.js';

  function coolInfo(s) {
    const bl = (new Date(s.breaker_until || 0) - Date.now()) / 1000;
    const dg = (new Date(s.degrade_until || 0) - Date.now()) / 1000;
    return Math.max(s.cool_remaining_sec || 0, bl > 0 ? bl : 0, dg > 0 ? dg : 0);
  }

  function statusOf(s) {
    const cool = coolInfo(s);
    if (s.disabled) return { cls: 'off', tag: 'bad', label: '已禁用' };
    if (cool > 0) {
      const bl = (new Date(s.breaker_until || 0) - Date.now()) / 1000;
      const dg = (new Date(s.degrade_until || 0) - Date.now()) / 1000;
      const kind =
        bl > Math.max(s.cool_remaining_sec || 0, dg > 0 ? dg : 0)
          ? '熔断'
          : dg > (s.cool_remaining_sec || 0)
            ? '连败降权'
            : s.cool_kind === 'hard_credit'
              ? '积分冷却'
              : '限流冷却';
      return { cls: 'cool', tag: 'warn', label: kind + ' · ' + dur(cool), frozen: true };
    }
    return { cls: '', tag: 'ok', label: '可用' };
  }

  function credTip(s, pct) {
    let tip =
      s.credits_total > 0 ? `剩余 ${s.credits} / 总额 ${s.credits_total}（${pct}%）` : '积分（相对池内最高）';
    const costs = (s.model_costs || []).filter((c) => c.model);
    if (costs.length) {
      tip += '\n实测单价（credits/1K）：\n' + costs.map((c) => '  ' + c.model + '：' + (c.cost_per_1k <= 0 ? '免费' : c.cost_per_1k)).join('\n');
    }
    return tip;
  }

  async function rowAct(a, s) {
    if (a === 'remove' && !confirm('移除账号将删除池状态与 auths/ 下的凭证文件，且不可恢复。确认移除？')) return;
    if (a === 'disable' && !confirm('禁用后该账号不再参与选号，需手动解冻才能恢复。确认禁用？')) return;
    const u = encodeURIComponent(s.uid);
    try {
      if (a === 'checkin') {
        const r = await api('accounts/' + u + '/checkin', { method: 'POST' });
        toast(
          '签到完成' +
            (r.credits != null ? '，积分 ' + r.credits + (r.credits_total > 0 ? '/' + r.credits_total : '') : '') +
            (r.checkin_message ? '（' + r.checkin_message + '）' : ''),
          'ok'
        );
      } else if (a === 'balance') {
        const r = await api('accounts/' + u + '/balance', { method: 'POST' });
        toast('余额已更新：' + r.credits + (r.credits_total > 0 ? ' / ' + r.credits_total : ''), 'ok');
      } else if (a === 'revive') {
        await api('accounts/' + u + '/revive', { method: 'POST' });
        toast('已解冻', 'ok');
      } else if (a === 'disable') {
        await api('accounts/' + u + '/disable', { method: 'POST' });
        toast('已禁用', 'ok');
      } else if (a === 'tasks') {
        ui.taskUid = s.uid;
        return;
      } else if (a === 'remove') {
        const r = await api('accounts/' + u + '/remove', { method: 'POST' });
        toast(r.file_error ? '已移除（凭证文件删除失败：' + r.file_error + '）' : '已移除', 'ok');
      }
    } catch (e) {
      if (e.unauthorized) authFailed();
      else toast(e.message, 'err');
    }
    dispatchEvent(new CustomEvent('wb:refresh'));
  }

  async function batch(path, label) {
    try {
      await api(path, { method: 'POST' });
      toast(label + '已开始，结果见日志', 'ok');
    } catch (e) {
      if (e.unauthorized) authFailed();
      else toast(e.message, 'err');
    }
  }

  let d = $derived(overview.data);
  let list = $derived(d?.accounts || []);
  let maxCred = $derived(Math.max(1, ...list.map((s) => s.credits || 0)));
  let remSum = $derived(list.reduce((a, s) => a + (s.credits || 0), 0));
  let totSum = $derived(list.reduce((a, s) => a + (s.credits_total || 0), 0));
</script>

{#if d}
  <div class="stats">
    <div class="stat"><div class="v">{d.total}</div><div class="k">账号总数</div></div>
    <div class="stat good"><div class="v">{d.healthy}</div><div class="k">可用</div></div>
    <div class="stat warn"><div class="v">{d.cooling}</div><div class="k">冷却中</div></div>
    <div class="stat bad"><div class="v">{d.disabled}</div><div class="k">已禁用</div></div>
    <div class="stat"><div class="v">{totSum > 0 ? remSum + ' / ' + totSum : remSum}</div><div class="k">积分剩余 / 总额</div></div>
    <div class="stat"><div class="v">{d.sticky_sessions}</div><div class="k">粘性会话</div></div>
  </div>

  <div class="box">
    <header>
      <h3>账号池</h3>
      <span class="grow"></span>
      <span class="note">{d.in_flight_full ? d.in_flight_full + ' 个账号在途占满' : ''}</span>
      <button class="btn xs" onclick={() => batch('checkin_all', '全部签到')}>全部签到</button>
      <button class="btn xs" onclick={() => batch('travel_all', '旅行巡检')}>旅行巡检</button>
      <button class="btn xs" onclick={() => batch('activity_all', '活跃上报')}>活跃上报</button>
      <button class="btn xs" onclick={() => batch('keepalive_all', '全部保活')}>全部保活</button>
    </header>
    <div class="tbl-wrap">
      <table class="acc">
        <thead>
          <tr>
            <th>账号</th>
            <th>状态</th>
            <th>积分</th>
            <th>成功 / 失败</th>
            <th>在途</th>
            <th>用量</th>
            <th>最近成功</th>
            <th class="c-acts"></th>
          </tr>
        </thead>
        <tbody>
          {#if !list.length}
            <tr>
              <td colspan="9">
                <div class="empty">
                  <div class="big">账号池是空的</div>
                  点击右上角「添加账号」，用浏览器登录一个 WorkBuddy 账号
                </div>
              </td>
            </tr>
          {:else}
            {#each list as s (s.uid)}
              {@const st = statusOf(s)}
              {@const pct =
                s.credits_total > 0
                  ? Math.min(100, Math.round(((s.credits || 0) / s.credits_total) * 100))
                  : Math.round(((s.credits || 0) / maxCred) * 100)}
              {@const tu = s.token_usage || {}}
              {@const req = tu.request_count || 0}
              {@const ttok = tu.total_tokens != null ? Number(tu.total_tokens) : null}
              <tr title={'uid: ' + s.uid}>
                <td class="who">
                  <div class="nm">
                    {#if s.nickname}{s.nickname}{:else}<span style="color: var(--ink-3);">未命名</span>{/if}
                    {#if s.realm === 'global'}<span class="realm-tag">国际版</span>{/if}
                  </div>
                  <div class="id">{shortUid(s.uid, 16)}{s.uid.length > 16 ? '…' : ''}</div>
                </td>
                <td>
                  <span class="tag {st.tag}">{st.label}</span>
                  {#if s.reason}<div class="hint mt-[3px] text-[11.5px]">{s.reason}</div>{/if}
                </td>
                <td class="cred" title={credTip(s, pct)}>
                  <div class="n">
                    {#if s.credits == null}—{:else}{s.credits}{#if s.credits_total > 0}<span class="of">/{s.credits_total}</span>{/if}{/if}
                  </div>
                  <div class="bar"><i style="width: {pct}%;"></i></div>
                </td>
                <td class="num">
                  {s.success_count || 0} <span style="color: var(--ink-3);">/</span>
                  <span style="color: var(--bad);">{s.err_total || 0}</span>
                </td>
                <td class="num">{s.in_flight || 0}</td>
                <td class="num" title="累计请求 {req} 次{ttok != null ? ' · ' + ttok + ' tok' : ''}">
                  <span class="tnum">{req} 次{#if ttok != null} · {ttok >= 1000 ? (ttok / 1000).toFixed(1) + 'k' : ttok}{/if}</span>
                </td>
                <td class="num" style="color: var(--ink-3);">{ago(s.last_success)}</td>
                <td class="acts">
                  <button class="btn xs ghost" onclick={() => rowAct('checkin', s)}>签到</button>
                  <button class="btn xs ghost" onclick={() => rowAct('balance', s)}>余额</button>
                  <button class="btn xs ghost" onclick={() => rowAct('tasks', s)}>任务</button>
                  {#if st.frozen || s.disabled}
                    <button class="btn xs primary" onclick={() => rowAct('revive', s)}>解冻</button>
                  {:else}
                    <button class="btn xs ghost" onclick={() => rowAct('disable', s)}>禁用</button>
                  {/if}
                  <button class="btn xs ghost danger" onclick={() => rowAct('remove', s)}>移除</button>
                </td>
              </tr>
            {/each}
          {/if}
        </tbody>
      </table>
    </div>
  </div>
{:else}
  <div class="box"><div class="empty"><span class="dots">加载中</span></div></div>
{/if}
