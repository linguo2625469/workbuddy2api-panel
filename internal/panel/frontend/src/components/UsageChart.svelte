<script>
  import { fmtTok, fmtInt, fmtDayLabel, fmtHourLabel } from '../lib/format.js';

  // points: 后端 series_window（新）或 series（旧）；
  // hours/windowFrom: 连续补零的依据；mode: tokens | requests。
  let { points = [], hours = 72, windowFrom = '', mode = 'tokens' } = $props();

  const W = 760;
  const H = 216;
  const PL = 54;
  const PR = 14;
  const PT = 14;
  const PB = 32;
  const IW = W - PL - PR;
  const IH = H - PT - PB;

  let tip = $state(null); // { i, left }

  function parseT(t) {
    const s = t.length === 13 ? t + ':00:00' : t + 'T00:00:00';
    const d = new Date(s);
    return Number.isNaN(d.getTime()) ? null : d.getTime();
  }

  function hourKey(t) {
    const d = new Date(t);
    const p = (n) => String(n).padStart(2, '0');
    return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}`;
  }

  function dayKey(t) {
    const d = new Date(t);
    const p = (n) => String(n).padStart(2, '0');
    return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}`;
  }

  function keyTime(k) {
    const d = new Date(k.length === 13 ? k + ':00:00' : k + 'T00:00:00');
    return d.getTime();
  }

  function niceCeil(v, integral = false) {
    if (!(v > 0)) return 1;
    const p = Math.pow(10, Math.floor(Math.log10(v)));
    const n = v / p;
    let m = n <= 1 ? 1 : n <= 2 ? 2 : n <= 2.5 ? (integral ? 5 : 2.5) : n <= 5 ? 5 : 10;
    return m * p;
  }

  let model = $derived.by(() => {
    const raw = [];
    for (const p of points) {
      const t = parseT(p.t);
      if (t === null) continue;
      raw.push({
        t,
        pt: Number(p.prompt_tokens || 0),
        ct: Number(p.completion_tokens || 0),
        req: Number(p.requests || 0),
        err: Number(p.errors || 0)
      });
    }
    if (!raw.length) return null;
    const daily = Number(hours) > 168;

    let start = Math.min(...raw.map((p) => p.t));
    if (windowFrom) {
      const w = new Date(windowFrom).getTime();
      if (!Number.isNaN(w)) start = Math.min(start, w);
    }

    const agg = new Map();
    for (const p of raw) {
      if (p.t < start - 1800000) continue; // 窗口起点之前的遗留日点不参与
      const k = daily ? dayKey(p.t) : hourKey(p.t);
      let a = agg.get(k);
      if (!a) {
        a = { t: keyTime(k), pt: 0, ct: 0, req: 0, err: 0 };
        agg.set(k, a);
      }
      a.pt += p.pt;
      a.ct += p.ct;
      a.req += p.req;
      a.err += p.err;
    }

    // 连续补零：空档读作零，而不是缺失（与旧版真实时间轴语义一致）。
    const step = daily ? 86400000 : 3600000;
    const endT = Math.max(...raw.map((p) => p.t), Date.now() - step);
    let slots = [];
    // 对齐到槽位边界
    let cur = daily ? keyTime(dayKey(start)) : keyTime(hourKey(start));
    const last = daily ? keyTime(dayKey(endT)) : keyTime(hourKey(endT));
    let guard = daily ? 95 : 800;
    while (cur <= last && guard-- > 0) {
      const k = daily ? dayKey(cur) : hourKey(cur);
      const a = agg.get(k);
      slots.push(a ? { ...a } : { t: cur, pt: 0, ct: 0, req: 0, err: 0, zero: true });
      cur += step;
    }
    // 小时槽过多（遗留 series 混入）时自动转日粒度，避免牙签柱。
    if (!daily && slots.length > 240) {
      const byDay = new Map();
      for (const s of slots) {
        const k = dayKey(s.t);
        let a = byDay.get(k);
        if (!a) {
          a = { t: keyTime(k), pt: 0, ct: 0, req: 0, err: 0 };
          byDay.set(k, a);
        }
        a.pt += s.pt;
        a.ct += s.ct;
        a.req += s.req;
        a.err += s.err;
      }
      slots = [...byDay.values()].sort((a, b) => a.t - b.t);
      return finish(slots, true);
    }
    return finish(slots, daily);
  });

  function finish(slots, daily) {
    const maxTok = Math.max(1, ...slots.map((s) => s.pt + s.ct));
    const maxReq = Math.max(1, ...slots.map((s) => s.req));
    const yMaxTok = niceCeil(maxTok);
    const yMaxReq = niceCeil(maxReq, true);
    const n = slots.length;
    const slotW = IW / Math.max(1, n);
    const bw = Math.max(2, Math.min(26, slotW * 0.62));
    const xOf = (i) => PL + i * slotW + (slotW - bw) / 2;

    // x 刻度：约 6 个等距槽位，时间感知标签。
    const want = Math.min(6, n);
    const idxs = [];
    for (let k = 0; k < want; k++) idxs.push(Math.round((k * (n - 1)) / Math.max(1, want - 1)));
    const ticks = [...new Set(idxs)].map((i) => {
      const s = slots[i];
      let lab;
      if (daily) lab = fmtDayLabel(s.t);
      else if (Number(hours) <= 24) lab = fmtHourLabel(s.t);
      else lab = fmtDayLabel(s.t) + ' ' + fmtHourLabel(s.t);
      return { i, x: PL + i * slotW + slotW / 2, lab };
    });

    return { slots, daily, maxTok, maxReq, yMaxTok, yMaxReq, n, slotW, bw, xOf, ticks };
  }

  function tipFor(s) {
    const d = new Date(s.t);
    const p = (n) => String(n).padStart(2, '0');
    const when = `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:00`;
    return { when, s };
  }
</script>

{#if !model}
  <div class="us-empty">暂无用量数据。发起一次对话后再刷新。</div>
{:else}
  {@const m = model}
  {@const reqMode = mode === 'requests'}
  {@const yMax = reqMode ? m.yMaxReq : m.yMaxTok}
  <svg viewBox="0 0 {W} {H}" role="img" aria-label="用量时序图" preserveAspectRatio="xMidYMid meet">
    {#each [0, 1, 2, 3, 4] as i}
      {@const y = PT + IH - (IH * i) / 4}
      <line class="gl" x1={PL} y1={y} x2={W - PR} y2={y} />
      <text class="tk" x={PL - 6} y={y + 3.5} text-anchor="end">
        {reqMode ? fmtInt((yMax * i) / 4) : fmtTok((yMax * i) / 4)}
      </text>
    {/each}

    {#if !reqMode}
      {@const areaPts = m.slots
        .map((s, i) => `${(PL + i * m.slotW + m.slotW / 2).toFixed(1)},${(PT + IH - IH * ((s.pt + s.ct) / yMax)).toFixed(1)}`)
        .join(' L ')}
      <path d="M {areaPts} L {(PL + IW).toFixed(1)},{PT + IH} L {PL},{PT + IH} Z" fill="var(--accent)" opacity="0.08" />
    {/if}

    {#each m.slots as s, i}
      {@const tt = s.pt + s.ct}
      {@const x = m.xOf(i)}
      {@const yBase = PT + IH}
      {#if !reqMode && tt > 0}
        {@const h = IH * (tt / yMax)}
        {@const hp = h * (s.pt / tt)}
        {@const hc = Math.max(1, h - hp)}
        {#if hp > 0}<rect x={x} y={yBase - hp} width={m.bw} height={hp} fill="var(--accent)" rx="1.5" />{/if}
        {#if s.ct > 0}<rect x={x} y={yBase - hp - hc} width={m.bw} height={hc} fill="var(--ok)" rx="1.5" />{/if}
      {:else if reqMode && s.req > 0}
        {@const h = Math.max(1.5, IH * (s.req / yMax))}
        <rect x={x} y={yBase - h} width={m.bw} height={h} fill="var(--accent)" opacity="0.8" rx="1.5" />
        {#if s.err > 0}
          <circle cx={x + m.bw / 2} cy={yBase - h - 5} r="3" fill="var(--bad)" />
        {/if}
      {/if}
      <title>
        {fmtDayLabel(s.t)}{m.daily ? '' : ' ' + fmtHourLabel(s.t)}：
        {reqMode
          ? `${fmtInt(s.req)} 次请求 / ${s.err} 失败`
          : `${fmtInt(s.pt)} prompt / ${fmtInt(s.ct)} completion / ${s.req} 次`}
      </title>
      <!-- svelte-ignore a11y_no_static_element_interactions -->
      <rect
        x={PL + i * m.slotW}
        y={PT}
        width={m.slotW}
        height={IH}
        fill="transparent"
        onmouseenter={() => {
          tip = { i, left: ((PL + i * m.slotW + m.slotW / 2) / W) * 100 };
        }}
        onmouseleave={() => (tip = null)}
        onfocus={() => {
          tip = { i, left: ((PL + i * m.slotW + m.slotW / 2) / W) * 100 };
        }}
        onblur={() => (tip = null)}
      />
    {/each}

    <line class="ax" x1={PL} y1={PT + IH} x2={W - PR} y2={PT + IH} />

    {#each m.ticks as t}
      <text
        class="tk"
        x={Math.max(PL, Math.min(W - PR, t.x))}
        y={PT + IH + 15}
        text-anchor={t.x < PL + 14 ? 'start' : t.x > W - PR - 14 ? 'end' : 'middle'}
      >
        {t.lab}
      </text>
    {/each}
  </svg>
{/if}

{#if tip && model && model.slots[tip.i]}
  {@const info = tipFor(model.slots[tip.i])}
  <div
    class="us-tip"
    style="left: min(max({tip.left}%, 8px), calc(100% - 270px)); top: 8px;"
  >
    <div class="tt">{info.when}{model.daily ? '（日）' : ''}</div>
    {#if mode === 'requests'}
      <div class="tr"><span class="k">请求</span><span class="v">{fmtInt(info.s.req)} 次</span></div>
      <div class="tr"><span class="k">失败</span><span class="v">{fmtInt(info.s.err)} 次</span></div>
      <div class="tr"><span class="k">Token</span><span class="v">{fmtInt(info.s.pt + info.s.ct)}</span></div>
    {:else}
      <div class="tr"><span class="k">prompt</span><span class="v">{fmtInt(info.s.pt)}</span></div>
      <div class="tr"><span class="k">completion</span><span class="v">{fmtInt(info.s.ct)}</span></div>
      <div class="tr"><span class="k">合计</span><span class="v">{fmtInt(info.s.pt + info.s.ct)}</span></div>
      <div class="tr"><span class="k">请求</span><span class="v">{fmtInt(info.s.req)} 次</span></div>
    {/if}
  </div>
{/if}
