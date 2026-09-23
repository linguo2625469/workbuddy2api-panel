<script>
  import { onMount } from 'svelte';
  import { api } from '../lib/api.js';
  import { ui, authFailed } from '../lib/stores.svelte.js';
  import { fmtTok } from '../lib/format.js';

  const PK_COLORS = [
    '#4f8cff', '#25b08b', '#e8a33d', '#c96bd6', '#e2607a',
    '#5aa9e6', '#8fbf3f', '#b58b5a', '#7d8fa8', '#d4785c'
  ];

  let list = $state([]);
  let phase = $state('idle'); // idle | loading | ready | error
  let errmsg = $state('');

  // 包按来源归并（code + name 双键：上游给「首登赠送」和普通活动包用过同名同码，
  // 只按 name 会把两类混成一类——两个号为何差 1500 就看不出来）。
  function pkBySource(packs) {
    const m = new Map();
    for (const p of packs || []) {
      const k = (p.package_code || '') + '|' + (p.name || '(未命名)');
      let e = m.get(k);
      if (!e) {
        e = { key: k, name: p.name || '(未命名)', code: p.package_code || '', n: 0, remain: 0, size: 0, used: 0, minEnd: '', minCreated: '' };
        m.set(k, e);
      }
      e.n += 1;
      e.remain += Number(p.remain || 0);
      e.size += Number(p.size || 0);
      e.used += Number(p.used || 0);
      const t = String(p.end_time || '').slice(0, 10);
      if (t && (!e.minEnd || t < e.minEnd)) e.minEnd = t;
      const c = String(p.created_at || '').slice(0, 10);
      if (c && (!e.minCreated || c < e.minCreated)) e.minCreated = c;
    }
    return [...m.values()].sort((a, b) => b.size - a.size);
  }

  let colorOf = $derived.by(() => {
    const names = [];
    for (const a of list) for (const s of pkBySource(a.packages || [])) if (!names.includes(s.key)) names.push(s.key);
    names.sort((x, y) => {
      const sz = (n) =>
        Math.max(
          ...list.map((a) => {
            const f = pkBySource(a.packages || []).find((s) => s.key === n);
            return f ? f.size : 0;
          })
        );
      return sz(y) - sz(x);
    });
    return (k) => PK_COLORS[names.indexOf(k) % PK_COLORS.length];
  });

  let maxRemain = $derived(Math.max(1, ...list.map((a) => Number(a.remain || 0))));

  async function load() {
    if (ui.route !== 'packages') return;
    phase = 'loading';
    try {
      const d = await api('packages');
      list = d.accounts || [];
      phase = 'ready';
    } catch (e) {
      if (e.unauthorized) authFailed();
      else {
        phase = 'error';
        errmsg = e.message;
      }
    }
  }

  onMount(() => {
    load();
    const h = () => {
      if (ui.route === 'packages' && phase !== 'loading') load();
    };
    addEventListener('wb:refresh', h);
    return () => removeEventListener('wb:refresh', h);
  });
</script>

<div class="box">
  <header>
    <h3>账号对比</h3>
    <span class="grow"></span>
    <span class="note">{phase === 'ready' ? list.length + ' 个账号 · 实时查询上游' : '—'}</span>
    <button class="btn xs" onclick={load}>刷新</button>
  </header>
  <div class="pad">
    {#if phase === 'loading' || phase === 'idle'}
      <div class="empty">查询中…（逐账号向上游实时查询）</div>
    {:else if phase === 'error'}
      <div class="empty">读取失败：{errmsg}</div>
    {:else if !list.length}
      <div class="empty">没有账号</div>
    {:else}
      <div class="pk-grid">
        {#each list as a (a.uid)}
          {#if a.error}
            <div class="pk-card">
              <div class="who">
                <span class="nm">{a.nickname || String(a.uid).slice(0, 8)}</span>
                <span class="realm">{a.realm || ''}</span>
              </div>
              <div class="err">查询失败：{a.error}</div>
            </div>
          {:else}
            {@const srcs = pkBySource(a.packages || [])}
            {@const total = Math.max(1, Number(a.size || 0))}
            <div class="pk-card">
              <div class="who">
                <span class="nm">{a.nickname || String(a.uid).slice(0, 8)}</span>
                <span class="realm">{a.realm || ''}</span>
              </div>
              <div class="big">{fmtTok(a.remain)}</div>
              <div class="sub">
                共 {fmtTok(a.size)} · {(a.packages || []).length} 个包 · 占最高 {(
                  (Number(a.remain || 0) / maxRemain) *
                  100
                ).toFixed(0)}%
              </div>
              <div class="mixbar">
                {#each srcs as s (s.key)}
                  <i style="width: {((s.size / total) * 100).toFixed(2)}%; background: {colorOf(s.key)};" title="{s.name} {fmtTok(s.size)}"></i>
                {/each}
              </div>
              <div class="pk-legend">
                {#each srcs as s (s.key)}
                  <span>
                    <i style="background: {colorOf(s.key)};"></i>
                    {s.name.replace(/^CodeBuddy/, '')} x{s.n} · {fmtTok(s.size)}{s.minCreated
                      ? ' · 首发 ' + s.minCreated.slice(5)
                      : ''}
                  </span>
                {/each}
              </div>
            </div>
          {/if}
        {/each}
      </div>
    {/if}
  </div>
</div>

{#if phase === 'ready'}
  {#each list as a (a.uid)}
    {#if !a.error}
      {@const packs = [...(a.packages || [])].sort((x, y) => Number(y.size || 0) - Number(x.size || 0))}
      <div class="box">
        <header>
          <h3>{a.nickname || String(a.uid).slice(0, 8)} · {a.realm || ''}</h3>
          <span class="grow"></span>
          <span class="note">余额 {fmtTok(a.remain)} / 总额 {fmtTok(a.size)} · {packs.length} 个包（按面额降序）</span>
        </header>
        <div class="tbl-wrap">
          <table class="acc">
            <thead>
              <tr>
                <th>包名 / 来源</th>
                <th class="num">面额</th><th class="num">剩余</th><th class="num">已用</th>
                <th class="num">发放</th><th class="num">到期</th>
              </tr>
            </thead>
            <tbody>
              {#each packs as p, i (p.package_code + '|' + (p.name || '') + '|' + i)}
                {@const k = (p.package_code || '') + '|' + (p.name || '(未命名)')}
                {@const sub =
                  String(p.sub_product_code || '').replace(/^sp_tcaca_codebuddyide_?/, '') ||
                  String(p.package_code || '').replace(/^TCACA_/, '')}
                <tr>
                  <td>
                    {p.name || '(未命名)'}
                    {#if sub}<div class="note">{sub}</div>{/if}
                  </td>
                  <td class="num">{fmtTok(p.size)}</td>
                  <td class="num">{fmtTok(p.remain)}</td>
                  <td class="num">{fmtTok(p.used)}</td>
                  <td class="num">{String(p.created_at || '').slice(0, 16).replace('T', ' ') || '—'}</td>
                  <td class="num">{String(p.end_time || '').slice(0, 10) || '—'}</td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      </div>
    {/if}
  {/each}
{/if}
