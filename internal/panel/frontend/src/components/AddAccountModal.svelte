<script>
  import { api } from '../lib/api.js';
  import { ui, toast } from '../lib/stores.svelte.js';
  import Modal from './Modal.svelte';

  let phase = $state('pick'); // pick | loading | ready | done | error
  let realm = $state('cn');
  let url = $state('');
  let msg = $state('');
  let pollTimer = $state(null);
  let loginState = $state(null);

  function reset() {
    phase = 'pick';
    url = '';
    msg = '';
    stopPoll();
  }

  function stopPoll() {
    if (pollTimer) {
      clearInterval(pollTimer);
      pollTimer = null;
    }
  }

  async function start() {
    phase = 'loading';
    try {
      const r = await api('login/start', { method: 'POST', body: { realm } });
      loginState = r.state;
      url = r.url;
      phase = 'ready';
      pollTimer = setInterval(poll, 3000);
    } catch (e) {
      phase = 'error';
      msg = e.message;
    }
  }

  async function poll() {
    if (!loginState) return;
    try {
      const r = await api('login/poll?state=' + encodeURIComponent(loginState));
      if (r.done) {
        stopPoll();
        phase = 'done';
        msg =
          '已添加 ' +
          (r.nickname || r.uid) +
          (r.realm === 'global' ? '（国际版）' : '') +
          (r.credits >= 0 ? ' · 积分 ' + r.credits + (r.credits_total > 0 ? '/' + r.credits_total : '') : '') +
          '，账号已载入池中';
        setTimeout(() => {
          ui.addOpen = false;
          reset();
          dispatchEvent(new CustomEvent('wb:refresh'));
        }, 1600);
      }
    } catch (e) {
      stopPoll();
      phase = 'error';
      msg = e.message + '（关闭后重新添加）';
    }
  }

  async function copy() {
    try {
      await navigator.clipboard.writeText(url);
      toast('链接已复制', 'ok');
    } catch {
      toast('复制失败，请手动选择复制', 'err');
    }
  }

  $effect(() => {
    if (!ui.addOpen) reset();
  });
</script>

<Modal
  open={ui.addOpen}
  title="添加账号"
  hint="浏览器完成腾讯账号登录，网关自动接续签到并载入账号池，无需重启。"
  onclose={() => {
    ui.addOpen = false;
  }}
>
  {#if phase === 'pick' || phase === 'loading'}
    <div class="flex items-center gap-3.5 px-0 py-1 text-[12.5px] text-ink2">
      <span class="hint">版本：</span>
      <label class="inline-flex cursor-pointer items-center gap-1.5">
        <input type="radio" name="addRealm" value="cn" bind:group={realm} class="accent-[var(--accent)]" /> 国内版（CN）
      </label>
      <label class="inline-flex cursor-pointer items-center gap-1.5">
        <input type="radio" name="addRealm" value="global" bind:group={realm} class="accent-[var(--accent)]" /> 国际版（Global）
      </label>
    </div>
    <div class="hint mt-0.5">国际版登录后，网关自动完成注册地区、激活与试用额度领取，全程无需手动操作。</div>
  {/if}
  {#if phase === 'loading'}
    <div class="state mt-3"><span class="dots">正在获取授权链接</span></div>
  {/if}
  {#if phase === 'ready'}
    <div class="state">在浏览器打开以下链接并登录：</div>
    <div class="url">{url}</div>
    <div class="state"><span class="dots">等待授权完成，自动检测中</span></div>
  {/if}
  {#if phase === 'done'}
    <div class="state ok">{msg}</div>
  {/if}
  {#if phase === 'error'}
    <div class="state err">{msg}</div>
  {/if}
  {#snippet footer()}
    <span class="grow"></span>
    {#if phase === 'pick' || phase === 'loading' || phase === 'error'}
      <button class="btn primary" onclick={start} disabled={phase === 'loading'}>
        {phase === 'error' ? '重试' : '获取授权链接'}
      </button>
    {/if}
    {#if phase === 'ready'}
      <button class="btn" onclick={copy}>复制链接</button>
      <button class="btn primary" onclick={() => open(url, '_blank')}>在浏览器打开</button>
    {/if}
    <button
      class="btn"
      onclick={() => {
        ui.addOpen = false;
      }}>关闭</button
    >
  {/snippet}
</Modal>
