<script>
  import { onMount } from 'svelte';
  import { api } from './lib/api.js';
  import { ui, overview, toast, authFailed, routeFromHash, TITLES, applyTheme, toggleTheme, effTheme } from './lib/stores.svelte.js';
  import Toasts from './components/Toasts.svelte';
  import KeyGate from './components/KeyGate.svelte';
  import AddAccountModal from './components/AddAccountModal.svelte';
  import TasksModal from './components/TasksModal.svelte';
  import VouchersModal from './components/VouchersModal.svelte';
  import Accounts from './views/Accounts.svelte';
  import Usage from './views/Usage.svelte';
  import Packages from './views/Packages.svelte';
  import TasksCenter from './views/TasksCenter.svelte';
  import Models from './views/Models.svelte';
  import Config from './views/Config.svelte';
  import Logs from './views/Logs.svelte';

  const NAV = ['accounts', 'usage', 'packages', 'taskscenter', 'models', 'config', 'logs'];

  let meta = $state('-');
  let refreshing = $state(false);

  async function loadOverview(quiet = true) {
    try {
      const d = await api('overview');
      overview.data = d;
      const up = Math.floor(d.uptime_sec || 0);
      meta =
        '运行 ' +
        (up >= 86400 ? Math.floor(up / 86400) + ' 天 ' : '') +
        Math.floor((up % 86400) / 3600) +
        ' 时 ' +
        Math.floor((up % 3600) / 60) +
        ' 分';
    } catch (e) {
      if (e.unauthorized) ui.keyOpen = true;
      else if (!quiet) toast(e.message, 'err');
    }
  }

  async function doRefresh() {
    if (refreshing) return;
    refreshing = true;
    try {
      await api('balance_all', { method: 'POST' });
      await loadOverview(true);
      toast('余额已从上游刷新', 'ok');
    } catch (e) {
      if (e.unauthorized) authFailed();
      else toast('刷新失败：' + e.message, 'err');
      await loadOverview(true);
    } finally {
      refreshing = false;
    }
    dispatchEvent(new CustomEvent('wb:refresh'));
  }

  function go(id) {
    location.hash = '#' + id;
  }

  onMount(() => {
    ui.route = routeFromHash();
    applyTheme();
    try {
      matchMedia('(prefers-color-scheme: light)').addEventListener('change', applyTheme);
    } catch {
      /* 忽略 */
    }
    const onhash = () => (ui.route = routeFromHash());
    const onref = () => loadOverview(true);
    addEventListener('hashchange', onhash);
    addEventListener('wb:refresh', onref);
    loadOverview(true);
    const t = setInterval(() => loadOverview(true), 5000);
    return () => {
      removeEventListener('hashchange', onhash);
      removeEventListener('wb:refresh', onref);
      clearInterval(t);
    };
  });

  let ov = $derived(overview.data);
  let navState = $derived(!ov ? '连接中' : ov.healthy > 0 ? '服务正常' : ov.total ? '无可用账号' : '待添加账号');
  let pulseCls = $derived(!ov || ov.healthy > 0 ? '' : ov.total ? ' warn' : ' bad');
</script>

<div class="min-h-screen">
  <!-- 顶栏：品牌 + 导航 tabs + 状态 + 动作 -->
  <header
    class="sticky top-0 z-30 border-b border-line bg-surface"
    style="backdrop-filter: blur(8px);"
  >
    <div class="mx-auto flex h-[60px] max-w-[1200px] items-center gap-5 px-4 md:px-6">
      <button onclick={() => go('accounts')} class="flex shrink-0 items-baseline gap-2 text-left">
        <span class="text-[15px] font-bold tracking-tight">WorkBuddy2API</span>
        <span class="font-mono text-[11px] text-ink3">{ov ? 'v' + ov.version : '控制台'}</span>
      </button>
      <nav class="hidden min-w-0 flex-1 items-center gap-0.5 overflow-x-auto lg:flex" aria-label="主导航">
        {#each NAV as id (id)}
          <button
            onclick={() => go(id)}
            class="relative shrink-0 px-3 py-2 text-[13.5px] {ui.route === id
              ? 'font-semibold text-ink'
              : 'text-ink2 hover:text-ink'}"
          >
            {TITLES[id]}
            {#if ui.route === id}
              <span class="absolute inset-x-3 -bottom-[13px] h-[2.5px] rounded-full bg-accent"></span>
            {/if}
          </button>
        {/each}
      </nav>
      <div class="grow lg:hidden"></div>
      <div class="hidden items-center gap-2 text-[12px] text-ink3 sm:flex">
        <span class="pulse{pulseCls}"></span><span class="tnum">{navState}</span>
      </div>
      <button
        class="btn icon ghost"
        onclick={toggleTheme}
        title={effTheme() === 'light' ? '切换到深色' : '切换到浅色'}
        aria-label="切换主题"
      >
        {#if effTheme() === 'light'}
          <svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" stroke-width="1.6">
            <circle cx="8" cy="8" r="3" /><path
              d="M8 1v2M8 13v2M1 8h2M13 8h2M3.2 3.2l1.4 1.4M11.4 11.4l1.4 1.4M12.8 3.2l-1.4 1.4M4.6 11.4l-1.4 1.4"
            />
          </svg>
        {:else}
          <svg viewBox="0 0 16 16" width="15" height="15" fill="none" stroke="currentColor" stroke-width="1.6">
            <path d="M13.2 9.6A5.6 5.6 0 0 1 6.4 2.8a5.6 5.6 0 1 0 6.8 6.8z" />
          </svg>
        {/if}
      </button>
      <button class="btn hidden sm:block" onclick={doRefresh} disabled={refreshing}>
        {refreshing ? '刷新中…' : '刷新'}
      </button>
      <button class="btn primary" onclick={() => (ui.addOpen = true)}>添加账号</button>
    </div>
    <!-- 移动端导航行 -->
    <nav class="flex gap-0.5 overflow-x-auto border-t border-linesoft px-3 py-1.5 lg:hidden" aria-label="主导航">
      {#each NAV as id (id)}
        <button
          onclick={() => go(id)}
          class="relative shrink-0 px-3 py-1.5 text-[13px] {ui.route === id ? 'font-semibold text-ink' : 'text-ink2'}"
        >
          {TITLES[id]}
          {#if ui.route === id}
            <span class="absolute inset-x-3 bottom-0 h-[2.5px] rounded-full bg-accent"></span>
          {/if}
        </button>
      {/each}
    </nav>
  </header>

  <main class="mx-auto w-full max-w-[1200px] px-4 pb-16 pt-6 md:px-6">
    <div class="mb-5 flex items-baseline gap-3">
      <h1 class="text-[22px] font-bold tracking-tight">{TITLES[ui.route]}</h1>
      <span class="font-mono text-[12px] text-ink3">{meta}</span>
      {#if ov}
        <span class="font-mono text-[12px] text-ink3">
          · {ov.healthy}/{ov.total} 可用{ov.redis_mode === 'upstash' ? ' · Redis 镜像' : ''}
        </span>
      {/if}
    </div>
    <div hidden={ui.route !== 'accounts'}><Accounts /></div>
    <div hidden={ui.route !== 'usage'}><Usage /></div>
    <div hidden={ui.route !== 'packages'}><Packages /></div>
    <div hidden={ui.route !== 'taskscenter'}><TasksCenter /></div>
    <div hidden={ui.route !== 'models'}><Models /></div>
    <div hidden={ui.route !== 'config'}><Config /></div>
    <div hidden={ui.route !== 'logs'}><Logs /></div>
  </main>
</div>

<KeyGate />
<AddAccountModal />
<TasksModal />
<VouchersModal />
<Toasts />
