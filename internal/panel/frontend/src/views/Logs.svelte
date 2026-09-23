<script>
  import { onMount } from 'svelte';
  import { api } from '../lib/api.js';
  import { ui, authFailed } from '../lib/stores.svelte.js';

  let ch = $state('all');
  let entries = $state([]);
  let counts = $state({ task: 0, chat: 0, sys: 0 });
  let pin = $state(true);
  let box = $state(null);

  const CH_NAME = { task: '任务', chat: '对话', sys: '系统' };

  function levelOf(text) {
    if (/error|失败|错误/.test(text)) return ' e';
    if (/warn|冷却|熔断/.test(text)) return ' w';
    return '';
  }

  async function load() {
    if (ui.route !== 'logs') return;
    const atEnd = box ? box.scrollTop + box.clientHeight >= box.scrollHeight - 24 : true;
    try {
      const d = await api('logs');
      const all = d.entries || [];
      entries = all.filter((e) => ch === 'all' || e.ch === ch);
      const c = {};
      for (const e of all) c[e.ch] = (c[e.ch] || 0) + 1;
      counts = { task: c.task || 0, chat: c.chat || 0, sys: c.sys || 0 };
      if (pin && atEnd && box) box.scrollTop = box.scrollHeight;
    } catch (e) {
      if (e.unauthorized) authFailed();
    }
  }

  function timeOf(ts) {
    if (!ts) return '';
    try {
      return new Date(ts).toLocaleTimeString('zh-CN', { hour12: false });
    } catch {
      return '';
    }
  }

  let note = $derived(
    ch === 'all'
      ? `任务 ${counts.task} · 对话 ${counts.chat} · 系统 ${counts.sys}`
      : (CH_NAME[ch] || ch) + ' ' + entries.length + ' 行'
  );

  onMount(() => {
    load();
    const h = () => load();
    addEventListener('wb:refresh', h);
    const t = setInterval(() => {
      if (ui.route === 'logs') load();
    }, 5000);
    return () => {
      removeEventListener('wb:refresh', h);
      clearInterval(t);
    };
  });
</script>

<div class="box">
  <header>
    <h3>运行日志</h3>
    <span class="grow"></span>
    <span class="chips">
      {#each [['all', '全部'], ['task', '任务'], ['chat', '对话'], ['sys', '系统']] as [v, label] (v)}
        <button
          class="btn xs chip {ch === v ? 'on' : ''}"
          onclick={() => {
            ch = v;
            load();
          }}>{label}</button
        >
      {/each}
    </span>
    <span class="note">最近 500 行 · {note}</span>
    <button class="btn xs" onclick={() => (pin = !pin)}>自动滚动：{pin ? '开' : '关'}</button>
  </header>
  <pre id="logBox" bind:this={box}>{#if !entries.length}<span style="color: var(--ink-3);">暂无日志</span>{:else}{#each entries as e, i (`${e.ts}:${i}`)}<span class="ln{levelOf(e.text)}">{#if ch === 'all'}<i class="lch c-{e.ch}">{CH_NAME[e.ch] || e.ch}</i>{/if}{timeOf(e.ts) + ' ' + e.text + '\n'}</span>{/each}{/if}</pre>
</div>
