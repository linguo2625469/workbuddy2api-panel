<script>
  import { api } from '../lib/api.js';
  import { ui, toast, authFailed } from '../lib/stores.svelte.js';
  import { qrMatrix, qrSVG, copyText } from '../lib/qr.js';
  import Modal from './Modal.svelte';

  let phase = $state('idle'); // idle | loading | ready | error
  let accounts = $state([]);
  let note = $state('');
  let errmsg = $state('');
  let openQR = $state(null); // 展开二维码的券码

  async function load() {
    phase = 'loading';
    openQR = null;
    try {
      const d = await api('school/vouchers');
      const arr = d.accounts || [];
      accounts = arr;
      const ok = arr.filter((a) => !a.error);
      const total = ok.reduce((n, a) => n + (a.vouchers || []).length, 0);
      note = total ? total + ' 张券 · ' + ok.filter((a) => !(a.vouchers || []).length).length + ' 个账号未抽中' : '';
      phase = 'ready';
    } catch (e) {
      if (e.unauthorized) {
        authFailed();
        return;
      }
      phase = 'error';
      errmsg = e.message;
    }
  }

  async function copy(code) {
    try {
      await copyText(code || '');
      toast('券码已复制', 'ok');
    } catch {
      toast('复制失败，请手动选择券码', 'err');
    }
  }

  function qrFor(code) {
    try {
      return qrSVG(qrMatrix(code), 148);
    } catch (e) {
      return '<span class="note">二维码生成失败：' + e.message + '</span>';
    }
  }

  $effect(() => {
    if (ui.vouchersOpen) load();
  });
</script>

<Modal
  open={ui.vouchersOpen}
  title="开学季 · 我的券码"
  hint="抽奖抽中的第三方券（KFC / 瑞幸 / 酷狗等），券码到对应 app/小程序兑换。"
  onclose={() => {
    ui.vouchersOpen = false;
  }}
>
  {#if phase === 'loading' || phase === 'idle'}
    <div class="state"><span class="dots">查询中</span></div>
  {:else if phase === 'error'}
    <div class="state err">{errmsg}</div>
  {:else}
    {@const ok = accounts.filter((a) => !a.error)}
    {@const withV = ok.filter((a) => (a.vouchers || []).length)}
    {#if !withV.length}
      <div class="empty"><div class="big">还没有抽到券</div></div>
    {:else}
      {#each withV as a (a.uid)}
        <div class="vc-acct">
          <span class="nm">{a.nickname || a.uid}</span>
          <span>{a.vouchers.length} 张</span>
        </div>
        {#each a.vouchers as v (v.code || v.prize_name)}
          {@const expired = v.valid_to && new Date(v.valid_to) < new Date()}
          <div class="vc{expired ? ' expired' : ''}">
            <div class="hd">
              <span class="nm">{v.prize_name || v.sku_code || '券'}</span>
              {#if expired}<span class="tag bad">已过期</span>{:else}<span class="tag ok">可使用</span>{/if}
            </div>
            <div class="meta">
              {v.valid_to ? '有效期至 ' + v.valid_to : '长期有效'}{v.granted_at
                ? ' · ' + String(v.granted_at).slice(0, 10) + ' 抽中'
                : ''}
            </div>
            <div class="sep"></div>
            <div class="ft">
              <span class="lab">券码</span><code>{v.code || '-'}</code>
              <span class="acts">
                {#if v.code}
                  <button class="btn xs ghost" onclick={() => (openQR = openQR === v.code ? null : v.code)}>二维码</button>
                {/if}
                <button class="btn xs ghost" onclick={() => copy(v.code)}>复制</button>
              </span>
            </div>
            {#if v.code && openQR === v.code}
              <div class="vc-qr">{@html qrFor(v.code)}</div>
            {/if}
          </div>
        {/each}
      {/each}
    {/if}
    {@const errs = accounts.filter((a) => a.error)}
    {#if errs.length}
      <div class="note mt-2" style="color: var(--warn);">
        查询失败：{errs.map((a) => (a.nickname || String(a.uid).slice(0, 8)) + '（' + a.error + '）').join('、')}
      </div>
    {/if}
  {/if}
  {#snippet footer()}
    <span class="hint">{note}</span>
    <span class="grow"></span>
    <button class="btn xs" onclick={load}>刷新</button>
    <button
      class="btn xs primary"
      onclick={() => {
        ui.vouchersOpen = false;
      }}>关闭</button
    >
  {/snippet}
</Modal>
