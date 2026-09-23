<script>
  import { api, setKey } from '../lib/api.js';
  import { ui, toast } from '../lib/stores.svelte.js';
  import Modal from './Modal.svelte';

  let val = $state('');
  let bad = $state(false);
  let busy = $state(false);

  async function enter() {
    const v = val.trim();
    if (!v || busy) return;
    busy = true;
    setKey(v);
    try {
      await api('overview');
      bad = false;
      ui.keyOpen = false;
      val = '';
      dispatchEvent(new CustomEvent('wb:refresh'));
    } catch {
      bad = true;
    } finally {
      busy = false;
    }
  }
</script>

<Modal open={ui.keyOpen} title="需要访问密钥" hint="该网关已启用 api_key 鉴权，请输入 config.json 中的密钥。" onclose={() => {}}>
  <input
    type="password"
    class="fld-input"
    placeholder="api_key"
    autocomplete="current-password"
    bind:value={val}
    onkeydown={(e) => {
      if (e.key === 'Enter') enter();
    }}
  />
  {#if bad}
    <div class="state err mt-2.5">密钥不正确，请重试。</div>
  {/if}
  {#snippet footer()}
    <span class="grow"></span>
    <button class="btn primary" onclick={enter} disabled={busy}>进入</button>
  {/snippet}
</Modal>
