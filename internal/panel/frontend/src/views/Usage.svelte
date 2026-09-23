<script>
  import { onMount } from 'svelte';
  import { api } from '../lib/api.js';
  import { ui, toast, authFailed } from '../lib/stores.svelte.js';
  import { fmtTok, fmtInt, fmtPct, fmtMs, fmtRate } from '../lib/format.js';
  import UsageChart from '../components/UsageChart.svelte';

  let data = $state(null);
  let err = $state('');
  let hours = $state('72');
  let mode = $state('tokens'); // tokens | requests
  let loading = $state(false);

  // 窗口口径优先（后端新字段），旧二进制回退累计口径。
  let w = $derived(data?.totals_window || data?.totals || {});
  let t = $derived(data?.totals || {});
  let hasWindow = $derived(!!data?.totals_window);
  let series = $derived(
    data?.series_window?.length ? data.series_window : data?.series || []
  );
  let accRows = $derived(
    hasWindow && data?.by_account_window?.length ? data.by_account_window : data?.by_account || []
  );
  let modelRows = $derived(
    hasWindow && data?.by_model_window?.length ? data.by_model_window : data?.by_model || []
  );
  let realmRows = $derived(
    hasWindow && data?.by_realm_window?.length ? data.by_realm_window : data?.by_realm || []
  );

  let scopeHint = $derived(
    !data ? '—' : `近 ${data.window_hours || hours} 小时 · ${(data.window_hours || 0) > 168 ? '日粒度' : '小时粒度'}`
  );
  let note = $derived(
    !data
      ? '—'
      : (hasWindow ? '卡片为窗口内合计' : '卡片为累计值（旧口径）') +
        ' · ' +
        (data.buckets || 0) +
        ' 个分桶' +
        (data.file_bytes ? ' · ' + (data.file_bytes / 1024).toFixed(1) + ' KB' : '')
  );
  let errRate = $derived(fmtPct(w.errors, w.requests));
  let pShare = $derived(fmtPct(w.prompt_tokens, w.total_tokens));
  let cShare = $derived(fmtPct(w.completion_tokens, w.total_tokens));

  function shareRow(a, denom) {
    const pct = denom > 0 ? (Number(a.total_tokens || 0) / denom) * 100 : 0;
    return { pct, label: pct < 0.05 && Number(a.total_tokens || 0) > 0 ? '<0.1%' : pct.toFixed(1) + '%' };
  }

  async function load() {
    if (ui.route !== 'usage') return;
    loading = true;
    try {
      data = await api('usage?hours=' + encodeURIComponent(hours));
      err = '';
    } catch (e) {
      if (e.unauthorized) authFailed();
      else err = e.message;
    } finally {
      loading = false;
    }
  }

  onMount(() => {
    load();
    const h = () => load();
    addEventListener('wb:refresh', h);
    return () => removeEventListener('wb:refresh', h);
  });
</script>

<div class="box">
  <header>
    <div>
      <div class="eyebrow">Usage / Windowed</div>
      <h3>用量总览 <span class="hint">{scopeHint}</span></h3>
    </div>
    <span class="grow"></span>
    <span class="note">{note}</span>
    <span class="seg" role="group" aria-label="图表视角">
      <button class="xs {mode === 'tokens' ? 'on' : ''}" aria-pressed={mode === 'tokens'} onclick={() => (mode = 'tokens')}>
        Token
      </button>
      <button class="xs {mode === 'requests' ? 'on' : ''}" aria-pressed={mode === 'requests'} onclick={() => (mode = 'requests')}>
        请求
      </button>
    </span>
    <select
      class="fld-input xs"
      style="width: auto; padding: 3.5px 9px; font-size: 12.5px;"
      bind:value={hours}
      onchange={load}
    >
      <option value="24">近 24 小时</option>
      <option value="72">近 3 天</option>
      <option value="168">近 7 天</option>
      <option value="720">近 30 天</option>
    </select>
    <button class="btn xs" onclick={load}>刷新</button>
  </header>
  <div class="pad">
    {#if err}
      <div class="state err">读取用量失败：{err}</div>
    {:else if !data}
      <div class="state"><span class="dots">加载中</span></div>
    {:else}
      <div class="us-hero">
        <div class="figure" title="精确值：{fmtInt(w.total_tokens)}">
          {fmtTok(w.total_tokens)}<span class="unit">tok</span>
        </div>
        <div class="qual">
          <div class="q1">
            近 {data.window_hours || hours} 小时 · {fmtInt(w.requests)} 次请求 · 失败 {fmtInt(w.errors)}（{errRate}）
          </div>
          <div class="q2">
            prompt {fmtInt(w.prompt_tokens)}（{pShare}） · completion {fmtInt(w.completion_tokens)}（{cShare}）
            · 均延迟 {fmtMs(w.avg_latency_ms)} · {fmtRate(w.avg_tokens_per_second)}
            · 累计 {fmtTok(t.total_tokens)}（自启用起）
          </div>
          <div class="split" aria-hidden="true">
            <i class="sp" style="width: {(Number(w.prompt_tokens || 0) / Math.max(1, Number(w.total_tokens || 0))) * 100}%;"></i>
            <i class="sc" style="width: {(Number(w.completion_tokens || 0) / Math.max(1, Number(w.total_tokens || 0))) * 100}%;"></i>
          </div>
        </div>
      </div>

      <div class="stats" id="usStats">
        <div class="stat"><div class="v" title={fmtInt(w.requests)}>{fmtTok(w.requests)}</div><div class="k">窗口请求</div><div class="sub">累计 {fmtInt(t.requests)}</div></div>
        <div class="stat"><div class="v" title={fmtInt(w.total_tokens)}>{fmtTok(w.total_tokens)}</div><div class="k">窗口 Token</div><div class="sub">累计 {fmtTok(t.total_tokens)}</div></div>
        <div class="stat"><div class="v" title={fmtInt(w.prompt_tokens)}>{fmtTok(w.prompt_tokens)}</div><div class="k">Prompt</div><div class="sub">占窗口 {pShare}</div></div>
        <div class="stat"><div class="v" title={fmtInt(w.completion_tokens)}>{fmtTok(w.completion_tokens)}</div><div class="k">Completion</div><div class="sub">占窗口 {cShare}</div></div>
        <div class="stat {w.errors ? 'warn' : ''}"><div class="v">{errRate}</div><div class="k">失败率</div><div class="sub">失败 {fmtInt(w.errors)} 次</div></div>
        <div class="stat"><div class="v">{fmtMs(w.avg_latency_ms)}</div><div class="k">均延迟</div><div class="sub">{fmtRate(w.avg_tokens_per_second)}</div></div>
      </div>

      <div class="uschart">
        <div class="uschart-hd">
          <span class="lb">{mode === 'tokens' ? 'Token 时序' : '请求时序'}</span>
          <span class="grow"></span>
          <span class="legend">
            {#if mode === 'tokens'}
              <i class="sw sw-p"></i>prompt {fmtTok(w.prompt_tokens)}<i class="sw sw-c"></i>completion {fmtTok(
                w.completion_tokens
              )}
            {:else}
              <i class="sw sw-p"></i>请求 {fmtInt(w.requests)}<i class="sw sw-e"></i>失败 {fmtInt(w.errors)}
            {/if}
          </span>
        </div>
        <div class="uschart-wrap">
          <div class="uschart-body">
            <UsageChart points={series} hours={Number(data.window_hours || hours)} windowFrom={data.window_from || ''} {mode} />
          </div>
        </div>
      </div>
    {/if}
  </div>
</div>

{#if data && !err}
  {@const denom = Math.max(1, Number(w.total_tokens || 0))}
  <div class="box">
    <header>
      <h3>按账号</h3><span class="grow"></span>
      <span class="note">{hasWindow ? '窗口内合计排序' : '累计排序'} · 请求数含失败尝试 · 悬停看精确值</span>
    </header>
    <div class="tbl-wrap">
      <table class="acc">
        <thead>
          <tr>
            <th>账号</th><th>域</th>
            <th class="num">请求</th><th class="num">失败</th>
            <th class="num">Prompt</th><th class="num">Completion</th><th class="num">合计</th>
            <th>占比</th>
            <th class="num">均延迟</th><th class="num">均速率</th>
          </tr>
        </thead>
        <tbody>
          {#if !accRows.length}
            <tr><td colspan="11" class="empty">暂无数据</td></tr>
          {:else}
            {#each accRows as x (x.key)}
              {@const sh = shareRow(x, denom)}
              <tr>
                <td title={x.key}>{String(x.key).slice(0, 8)}{#if x.extra}<div class="note">{x.extra}</div>{/if}</td>
                <td class="num">{x.realm || ''}</td>
                <td class="num" title={fmtInt(x.requests)}>{fmtTok(x.requests)}</td>
                <td class="num" title={fmtInt(x.errors)}>
                  {#if x.errors}<span style="color: var(--warn);">{fmtTok(x.errors)}</span>{:else}—{/if}
                </td>
                <td class="num" title={fmtInt(x.prompt_tokens)}>{fmtTok(x.prompt_tokens)}</td>
                <td class="num" title={fmtInt(x.completion_tokens)}>{fmtTok(x.completion_tokens)}</td>
                <td class="num" title={fmtInt(x.total_tokens)}>{fmtTok(x.total_tokens)}</td>
                <td>
                  <span class="us-share" title={fmtInt(x.total_tokens) + ' / 窗口 ' + fmtInt(denom)}>
                    <span class="track"><i style="width: {Math.min(100, sh.pct)}%;"></i></span>
                    <span class="pc">{sh.label}</span>
                  </span>
                </td>
                <td class="num">{fmtMs(x.avg_latency_ms)}</td>
                <td class="num">{fmtRate(x.avg_tokens_per_second)}</td>
              </tr>
            {/each}
          {/if}
        </tbody>
      </table>
    </div>
  </div>

  <div class="box">
    <header><h3>按模型</h3><span class="grow"></span><span class="note">{hasWindow ? '窗口内合计排序' : '累计排序'}</span></header>
    <div class="tbl-wrap">
      <table class="acc">
        <thead>
          <tr>
            <th>模型</th>
            <th class="num">请求</th><th class="num">失败</th>
            <th class="num">Prompt</th><th class="num">Completion</th><th class="num">合计</th>
            <th>占比</th>
          </tr>
        </thead>
        <tbody>
          {#if !modelRows.length}
            <tr><td colspan="8" class="empty">暂无数据</td></tr>
          {:else}
            {#each modelRows as x (x.key)}
              {@const sh = shareRow(x, denom)}
              <tr>
                <td title={x.key}>{x.key}</td>
                <td class="num" title={fmtInt(x.requests)}>{fmtTok(x.requests)}</td>
                <td class="num" title={fmtInt(x.errors)}>
                  {#if x.errors}<span style="color: var(--warn);">{fmtTok(x.errors)}</span>{:else}—{/if}
                </td>
                <td class="num" title={fmtInt(x.prompt_tokens)}>{fmtTok(x.prompt_tokens)}</td>
                <td class="num" title={fmtInt(x.completion_tokens)}>{fmtTok(x.completion_tokens)}</td>
                <td class="num" title={fmtInt(x.total_tokens)}>{fmtTok(x.total_tokens)}</td>
                <td>
                  <span class="us-share" title={fmtInt(x.total_tokens) + ' / 窗口 ' + fmtInt(denom)}>
                    <span class="track"><i style="width: {Math.min(100, sh.pct)}%;"></i></span>
                    <span class="pc">{sh.label}</span>
                  </span>
                </td>
              </tr>
            {/each}
          {/if}
        </tbody>
      </table>
    </div>
  </div>

  <div class="box">
    <header><h3>按域</h3><span class="grow"></span><span class="note">{hasWindow ? '窗口内合计排序' : '累计排序'}</span></header>
    <div class="tbl-wrap">
      <table class="acc">
        <thead>
          <tr>
            <th>realm</th>
            <th class="num">请求</th><th class="num">失败</th>
            <th class="num">Prompt</th><th class="num">Completion</th><th class="num">合计</th>
            <th>占比</th>
          </tr>
        </thead>
        <tbody>
          {#if !realmRows.length}
            <tr><td colspan="8" class="empty">暂无数据</td></tr>
          {:else}
            {#each realmRows as x (x.key)}
              {@const sh = shareRow(x, denom)}
              <tr>
                <td>{x.key}</td>
                <td class="num" title={fmtInt(x.requests)}>{fmtTok(x.requests)}</td>
                <td class="num" title={fmtInt(x.errors)}>
                  {#if x.errors}<span style="color: var(--warn);">{fmtTok(x.errors)}</span>{:else}—{/if}
                </td>
                <td class="num" title={fmtInt(x.prompt_tokens)}>{fmtTok(x.prompt_tokens)}</td>
                <td class="num" title={fmtInt(x.completion_tokens)}>{fmtTok(x.completion_tokens)}</td>
                <td class="num" title={fmtInt(x.total_tokens)}>{fmtTok(x.total_tokens)}</td>
                <td>
                  <span class="us-share" title={fmtInt(x.total_tokens) + ' / 窗口 ' + fmtInt(denom)}>
                    <span class="track"><i style="width: {Math.min(100, sh.pct)}%;"></i></span>
                    <span class="pc">{sh.label}</span>
                  </span>
                </td>
              </tr>
            {/each}
          {/if}
        </tbody>
      </table>
    </div>
  </div>
{/if}
