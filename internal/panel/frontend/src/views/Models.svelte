<script>
  import { onMount } from 'svelte';
  import { api } from '../lib/api.js';
  import { ui, authFailed } from '../lib/stores.svelte.js';

  let rows = $state([]);
  let note = $state('实时查询上游');
  let phase = $state('idle');

  function fmtK(n) {
    n = Number(n || 0);
    return n >= 1000 ? Math.round(n / 1000) + 'K' : String(n);
  }

  function probeDays(ts) {
    if (!ts) return null;
    const t = new Date(String(ts).replace(' ', 'T'));
    const d = (Date.now() - t.getTime()) / 86400000;
    return Number.isNaN(d) ? null : Math.floor(d);
  }

  // 实测上限单元格：声称 vs 实测，钳制标黄告警（probe 工具写入的数据文件）。
  function outCell(m, pr) {
    if (!pr)
      return { html: `<td class="num">${m.max_output_tokens ? fmtK(m.max_output_tokens) : '—'}</td>` };
    const tip =
      '声称 ' +
      (pr.claimed ? fmtK(pr.claimed) : '?') +
      ' · 实测 ' +
      (pr.measured ? fmtK(pr.measured) : '?') +
      (pr.note ? ' · ' + pr.note : '') +
      (pr.tested_at ? ' · 探测于 ' + pr.tested_at : '');
    const days = probeDays(pr.tested_at);
    const stale = days !== null && days > 30 ? ' · ' + days + ' 天前' : '';
    if (pr.verdict === 'clamped' && pr.measured) {
      if (pr.claimed && pr.measured < pr.claimed) {
        const x = pr.claimed / pr.measured;
        const xs = (x >= 10 ? Math.round(x) : Math.round(x * 10) / 10) + '×';
        return {
          html:
            `<td class="num" title="${tip}"><span style="color:var(--warn);font-weight:600">` +
            fmtK(pr.measured) +
            ` ⚠</span><div class="note">钳制 ${xs}${stale}</div></td>`
        };
      }
      return {
        html:
          `<td class="num" title="${tip}"><span style="color:var(--ok)">` +
          fmtK(pr.measured) +
          (pr.claimed && pr.measured > pr.claimed ? ' ↑' : ' ✓') +
          '</span></td>'
      };
    }
    if (pr.verdict === 'at_least' && pr.measured)
      return { html: `<td class="num" title="${tip}"><span style="color:var(--ink-3)">≥${fmtK(pr.measured)}</span></td>` };
    return { html: `<td class="num" title="${tip}"><span style="color:var(--ink-3)">?</span><div class="note">未测出${stale}</div></td>` };
  }

  async function load() {
    if (ui.route !== 'models') return;
    phase = 'loading';
    try {
      const [d, pr] = await Promise.all([api('models'), api('model_probes').catch(() => ({}))]);
      const list = d.models || [];
      if (!list.length) {
        phase = 'empty';
        return;
      }
      const probes = pr.probes || {};
      const keys = Object.keys(probes);
      const probeOf = (id) => probes[id] || probes[keys.find((k) => k.endsWith(':' + id))];
      rows = list.map((m) => {
        const eff = [...(m.supported_efforts || [])];
        if (m.can_disable_thinking && eff.length && !eff.includes('off')) eff.push('off（可关）');
        const caps = [];
        if (m.is_default) caps.push(['ok', '默认']);
        if (m.supports_tool_call) caps.push(['warn', '工具']);
        if (m.supports_images) caps.push(['warn', '视觉']);
        if (m.supports_reasoning && !m.can_disable_thinking) caps.push(['warn', '思考常开']);
        return { m, eff, caps, out: outCell(m, probeOf(m.id)) };
      });
      const hit = list.filter((m) => probeOf(m.id)).length;
      note = list.length + ' 个模型 · 已刷新降级缓存' + (hit ? ' · ' + hit + ' 个有实测上限' : '');
      phase = 'ready';
    } catch (e) {
      if (e.unauthorized) authFailed();
      else {
        phase = 'error';
        note = e.message;
      }
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
    <h3>
      模型能力 <span class="hint">max_tokens bug 已修复目前可以正常透传最大输出，但是官方返回的最大输出仅为参考，不保证完全正确</span>
    </h3>
    <span class="grow"></span>
    <span class="note">{note}</span>
    <button class="btn xs" onclick={load}>重新获取</button>
  </header>
  <div class="tbl-wrap">
    <table class="acc md">
      <thead>
          <tr>
            <th>模型</th><th>积分倍率</th><th>默认档</th>
          <th>支持的思考档位</th><th>上下文长度</th><th>最大输出</th>
        </tr>
      </thead>
      <tbody>
        {#if phase === 'loading' || phase === 'idle'}
          <tr><td colspan="7"><div class="empty">正在向上游查询…</div></td></tr>
        {:else if phase === 'empty'}
          <tr><td colspan="7"><div class="empty">上游未返回模型</div></td></tr>
        {:else if phase === 'error'}
          <tr><td colspan="7"><div class="empty">{note}</div></td></tr>
        {:else}
          {#each rows as r (r.m.id)}
            <tr>
              <td class="who" title={r.m.description || ''}>
                <div class="nm">{r.m.id}</div>
                <div class="id">{r.m.name || ''}</div>
                {#if r.caps.length}
                  <div class="id mt-0.5">
                    {#each r.caps as [cls, label] (label)}<span class="tag {cls}">{label}</span>{' '}{/each}
                  </div>
                {/if}
              </td>
              <td class="num">{r.m.credits || '—'}</td>
              <td>
                {#if r.m.default_effort}<span class="tag ok">{r.m.default_effort}</span>{:else}<span style="color: var(--ink-3);">—</span>{/if}
              </td>
              <td class="efs" style="white-space: normal;">
                {#if r.eff.length}
                  {#each r.eff as e (e)}<span class="tag warn">{e}</span>{' '}{/each}
                {:else}
                  <span style="color: var(--ink-3); font-size: 12.5px;">
                    {r.m.supports_reasoning ? '固定档 · 默认 ' + (r.m.default_effort || '?') : '不支持思考'}
                  </span>
                {/if}
              </td>
              <td class="num">{r.m.context_length ? Math.round(r.m.context_length / 1000) + 'K' : '—'}</td>
              {@html r.out.html}
            </tr>
          {/each}
        {/if}
      </tbody>
    </table>
  </div>
</div>
