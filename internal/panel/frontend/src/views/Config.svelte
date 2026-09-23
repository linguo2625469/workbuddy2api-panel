<script>
  import { onMount } from 'svelte';
  import { api } from '../lib/api.js';
  import { ui, toast, authFailed } from '../lib/stores.svelte.js';

  const CFG_MAP = {
    listen: ['listen'],
    api_key: ['api_key'],
    checkin_hours: ['schedule', 'checkin_hours'],
    checkin_enabled: ['schedule', 'checkin_enabled'],
    travel_hours: ['schedule', 'travel_hours'],
    travel_enabled: ['schedule', 'travel_enabled'],
    activity_hours: ['schedule', 'activity_hours'],
    activity_enabled: ['schedule', 'activity_enabled'],
    keepalive_hours: ['schedule', 'keepalive_hours'],
    keepalive_enabled: ['schedule', 'keepalive_enabled'],
    balance_refresh_enabled: ['schedule', 'balance_refresh_enabled'],
    balance_refresh_minutes: ['schedule', 'balance_refresh_minutes'],
    max_in_flight: ['pool', 'max_in_flight'],
    max_in_flight_global: ['pool', 'max_in_flight_global'],
    breaker_threshold: ['pool', 'breaker_threshold'],
    degrade_threshold: ['pool', 'degrade_threshold'],
    degrade_cooldown: ['pool', 'degrade_cooldown'],
    degrade_cooldown_max: ['pool', 'degrade_cooldown_max'],
    cost_explore_interval: ['pool', 'cost_explore_interval'],
    soft_rate: ['cooldown', 'soft_rate'],
    soft_rate_max: ['cooldown', 'soft_rate_max'],
    breaker_cooldown: ['pool', 'breaker_cooldown'],
    breaker_cooldown_max: ['pool', 'breaker_cooldown_max'],
    idle_weight_per_hour: ['pool', 'idle_weight_per_hour'],
    idle_weight_max: ['pool', 'idle_weight_max'],
    ttl: ['session_sticky', 'ttl'],
    timeout_seconds: ['upstream', 'timeout_seconds'],
    header_timeout_seconds: ['upstream', 'header_timeout_seconds'],
    idle_timeout_seconds: ['upstream', 'idle_timeout_seconds'],
    user_agent: ['upstream', 'user_agent'],
    prompt_mode: ['prompt', 'mode'],
    prompt_file: ['prompt', 'file'],
    sanitize_blacklist_fingerprints: ['features', 'sanitize_blacklist_fingerprints'],
    session_sticky_enabled: ['session_sticky', 'enabled']
  };

  // Go 时长口径（与后端 time.ParseDuration 同口径）：空 = 沿用现值不发送。
  const DURATION_RE = /^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/;
  const DURATION_FIELDS = [
    'soft_rate', 'soft_rate_max', 'breaker_cooldown', 'breaker_cooldown_max',
    'degrade_cooldown', 'degrade_cooldown_max', 'cost_explore_interval', 'ttl'
  ];
  const DURATION_TIP = '格式应为 Go 时长：30m / 2h / 600s / 1h30m';

  // form 持有全部字段的显示值（字符串 / 布尔），回填与收集都走这里。
  let form = $state({});
  let cfgPath = $state('');
  let note = $state('');
  let saving = $state(false);
  let showKey = $state(false);

  function dig(obj, path) {
    return path.reduce((o, k) => (o == null ? undefined : o[k]), obj);
  }
  function put(obj, path, val) {
    let o = obj;
    for (let i = 0; i < path.length - 1; i++) {
      if (typeof o[path[i]] !== 'object' || o[path[i]] === null) o[path[i]] = {};
      o = o[path[i]];
    }
    o[path[path.length - 1]] = val;
  }

  function badOf(name) {
    const v = String(form[name] ?? '').trim();
    return v !== '' && !DURATION_RE.test(v);
  }

  async function load() {
    if (ui.route !== 'config') return;
    try {
      const d = await api('config');
      const cfg = d.config || {};
      cfgPath = d.path || '';
      const next = {};
      for (const [name, path] of Object.entries(CFG_MAP)) {
        const v = dig(cfg, path);
        const cur = form[name];
        // checkbox 存布尔，其余存字符串；数组 join 回填。
        if (typeof cur === 'boolean' || name.endsWith('_enabled') || name === 'sanitize_blacklist_fingerprints') {
          next[name] = !!v;
        } else if (Array.isArray(v)) next[name] = v.join(', ');
        else next[name] = v == null ? '' : String(v);
      }
      form = next;
      note = '';
    } catch (e) {
      if (e.unauthorized) authFailed();
      else toast('读取配置失败：' + e.message, 'err');
    }
  }

  function collect() {
    const out = {};
    for (const [name, path] of Object.entries(CFG_MAP)) {
      const el = form[name];
      if (el === undefined) continue;
      let v;
      if (typeof el === 'boolean') v = el;
      else {
        const raw = String(el).trim();
        if (raw === '') continue;
        if (name.endsWith('_hours')) v = raw.split(/[,，\s]+/).filter(Boolean).map(Number);
        else if (['max_in_flight', 'max_in_flight_global', 'breaker_threshold', 'degrade_threshold', 'balance_refresh_minutes', 'timeout_seconds', 'header_timeout_seconds', 'idle_timeout_seconds'].includes(name)) {
          if (raw === '') continue;
          v = Number(raw);
        } else v = raw;
      }
      put(out, path, v);
    }
    return out;
  }

  const LABELS = {
    soft_rate: '软限流冷却基数', soft_rate_max: '软冷却退避上限', breaker_cooldown: '熔断基础时长',
    breaker_cooldown_max: '熔断退避上限', degrade_cooldown: '连败降权时长', degrade_cooldown_max: '连败降权上限',
    cost_explore_interval: '成本探索窗口', ttl: '会话粘性 TTL'
  };

  async function save() {
    const firstBad = DURATION_FIELDS.find(badOf);
    if (firstBad) {
      toast('「' + (LABELS[firstBad] || firstBad) + '」' + DURATION_TIP, 'err');
      return;
    }
    saving = true;
    try {
      const r = await api('config', { method: 'POST', body: collect() });
      const n = (r.restart_required || []).length;
      toast(n ? '配置已保存，其中 ' + n + ' 项需重启进程生效' : '配置已保存并立即生效', 'ok');
      const k = String(form.api_key || '').trim();
      if (k) {
        try {
          localStorage.setItem('wb2api.key', k);
        } catch {
          /* 忽略 */
        }
      }
      load();
      dispatchEvent(new CustomEvent('wb:refresh'));
    } catch (e) {
      if (e.unauthorized) authFailed();
      else toast('保存失败：' + e.message, 'err');
    } finally {
      saving = false;
    }
  }

  onMount(() => {
    // 初始化布尔键，避免 checkbox 非受控告警。
    for (const name of Object.keys(CFG_MAP)) {
      if (name.endsWith('_enabled') || name === 'sanitize_blacklist_fingerprints') form[name] = false;
      else if (!(name in form)) form[name] = '';
    }
    load();
    const h = () => load();
    addEventListener('wb:refresh', h);
    return () => removeEventListener('wb:refresh', h);
  });
</script>

{#snippet field(name, label, hint = '', type = 'text', extra = '')}
  <label class="mb-3.5 block">
    <span class="mb-[5px] block text-[12.5px] text-ink2">{label}</span>
    {#if name === 'prompt_mode'}
      <select class="fld-input" bind:value={form[name]}>
        <option value="custom">custom — 网关自有提示词（避免指纹误报）</option>
        <option value="append">append — 客户端 system 后插网关提示词（并用）</option>
        <option value="passthrough">passthrough — 透传客户端原始 system</option>
      </select>
    {:else}
      <input
        class="fld-input {badOf(name) ? 'invalid' : ''}"
        title={badOf(name) ? DURATION_TIP : ''}
        {type}
        min={type === 'number' ? 0 : undefined}
        step={name.startsWith('idle_weight') ? '0.1' : undefined}
        placeholder={extra}
        bind:value={form[name]}
      />
    {/if}
    {#if hint}<span class="hint mt-1 block">{hint}</span>{/if}
  </label>
{/snippet}

{#snippet switchRow(name, label)}
  <div class="flex items-center justify-between py-2">
    <span class="text-[13.5px]">{label}</span>
    <input type="checkbox" class="accent-[var(--accent)]" bind:checked={form[name]} />
  </div>
{/snippet}

<div class="box">
  <header><h3>服务</h3><span class="grow"></span><span class="note">{cfgPath}</span></header>
  <div class="pad">
    <div class="grid gap-x-5 md:grid-cols-2">
      {@render field('listen', '监听地址', '改动需重启进程', 'text', ':7863')}
      <label class="mb-3.5 block">
        <span class="mb-[5px] block text-[12.5px] text-ink2">API 密钥</span>
        <div class="flex items-center gap-2">
          <input
            class="fld-input"
            type={showKey ? 'text' : 'password'}
            placeholder="留空 = 不鉴权"
            bind:value={form.api_key}
          />
          <button type="button" class="btn xs" onclick={() => (showKey = !showKey)}>{showKey ? '隐藏' : '显示'}</button>
        </div>
        <span class="hint mt-1 block">立即生效（含面板自身）</span>
      </label>
    </div>
  </div>
</div>

<div class="box">
  <header><h3>定时任务</h3></header>
  <div class="pad">
    <div class="grid gap-x-5 md:grid-cols-2">
      <div>
        {@render switchRow('checkin_enabled', '自动签到')}
        {@render field('checkin_hours', '签到时点（小时，逗号分隔）', '', 'text', '9, 21')}
      </div>
      <div>
        {@render switchRow('keepalive_enabled', 'Token 保活')}
        {@render field('keepalive_hours', '保活时点（小时，逗号分隔）', '', 'text', '22')}
      </div>
    </div>
    <div class="my-4 h-px" style="background: var(--line-soft);"></div>
    <div class="grid gap-x-5 md:grid-cols-2">
      <div>
        {@render switchRow('travel_enabled', '猫猫旅行')}
        {@render field('travel_hours', '旅行时点（小时，逗号分隔）', '一趟派出 + 一趟领奖闭环', 'text', '9, 21')}
      </div>
      <div>
        {@render switchRow('activity_enabled', '活跃上报')}
        {@render field('activity_hours', '上报时点（小时，逗号分隔）', '点亮连登 + 解锁领养前置', 'text', '10')}
      </div>
    </div>
    <div class="my-4 h-px" style="background: var(--line-soft);"></div>
    <div class="grid gap-x-5 md:grid-cols-2">
      {@render switchRow('balance_refresh_enabled', '后台刷新余额')}
      {@render field('balance_refresh_minutes', '刷新间隔（分钟）', '', 'number', '5')}
    </div>
  </div>
</div>

<div class="box">
  <header><h3>账号池与流量治理</h3></header>
  <div class="pad">
    <div class="grid gap-x-4 md:grid-cols-3">
      {@render field('max_in_flight', '单账号最大在途', '0 = 不限制', 'number', '3')}
      {@render field('max_in_flight_global', '国际版在途上限', 'global 域风控更紧，默认 2', 'number', '2')}
      {@render field('breaker_threshold', '连续失败熔断阈值', '', 'number', '3')}
    </div>
    <div class="grid gap-x-4 md:grid-cols-3">
      {@render field('soft_rate', '软限流冷却基数', '', 'text', '600s')}
      {@render field('soft_rate_max', '软冷却退避上限', '', 'text', '2h')}
      {@render field('breaker_cooldown', '熔断基础时长', '', 'text', '30m')}
    </div>
    <div class="grid gap-x-4 md:grid-cols-3">
      {@render field('breaker_cooldown_max', '熔断退避上限', '', 'text', '6h')}
      {@render field('degrade_threshold', '连败降权阈值', '未知错误连败 N 次临时出池', 'number', '5')}
      {@render field('degrade_cooldown', '连败降权时长', '', 'text', '10m')}
    </div>
    <div class="grid gap-x-4 md:grid-cols-3">
      {@render field('degrade_cooldown_max', '连败降权上限', '', 'text', '2h')}
      {@render field('idle_weight_per_hour', '闲置补偿 / 小时', '', 'number', '0.5')}
      {@render field('idle_weight_max', '闲置补偿上限', '', 'number', '5')}
    </div>
    <div class="grid gap-x-4 md:grid-cols-3">
      {@render field('cost_explore_interval', '成本探索窗口', '垄断破除：免费层垄断时定期搭车探索未知号；0 关停', 'text', '30m')}
      {@render field('ttl', '会话粘性 TTL', '改动需重启进程', 'text', '30m')}
    </div>
  </div>
</div>

<div class="box">
  <header><h3>上游与高级</h3></header>
  <div class="pad">
    <div class="grid gap-x-4 md:grid-cols-3">
      {@render field('timeout_seconds', '短请求超时', '需重启', 'number', '120')}
      {@render field('header_timeout_seconds', '聊天首字节超时', '需重启', 'number', '120')}
      {@render field('idle_timeout_seconds', '流空闲超时', '需重启', 'number', '300')}
    </div>
    <div class="my-4 h-px" style="background: var(--line-soft);"></div>
    <div class="grid gap-x-5 md:grid-cols-2">
      {@render field('user_agent', '出站 User-Agent', '影响官网积分记录「使用端」显示；需重启', 'text', '留空 = CLI/2.63.2 CodeBuddy/2.63.2')}
    </div>
    <div class="my-4 h-px" style="background: var(--line-soft);"></div>
    <div class="grid gap-x-5 md:grid-cols-2">
      {@render field('prompt_mode', '系统提示词模式', '需重启')}
      {@render field('prompt_file', '提示词文件路径', '需重启', 'text', '留空 = 内置默认提示词')}
    </div>
    <div class="my-4 h-px" style="background: var(--line-soft);"></div>
    {@render switchRow('sanitize_blacklist_fingerprints', '出站请求指纹脱敏')}
    {@render switchRow('session_sticky_enabled', '会话粘性路由')}
    <div class="hint mt-2.5">Upstash Redis 镜像、凭证目录与状态文件路径需手工编辑配置文件（判为重启项）。</div>
  </div>
</div>

<div
  class="sticky bottom-0 flex items-center gap-3 border-t border-linesoft px-[17px] py-3"
  style="background: var(--surface-2);"
>
  <span class="grow"></span>
  <span class="text-[12.5px]" style="color: var(--warn);">{note}</span>
  <button type="button" class="btn" onclick={load}>放弃修改</button>
  <button type="button" class="btn primary" onclick={save} disabled={saving}>{saving ? '保存中…' : '保存配置'}</button>
</div>
