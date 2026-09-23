<script>
  import { toasts } from '../lib/stores.svelte.js';

  function edgeColor(cls) {
    if (cls === 'ok') return 'var(--ok)';
    if (cls === 'err') return 'var(--bad)';
    return 'var(--ink-3)';
  }
</script>

<div class="fixed bottom-5 right-5 z-[90] flex flex-col items-end gap-2" aria-live="polite">
  {#each toasts as t (t.id)}
    <div
      class="max-w-[380px] rounded-lg border border-line bg-raise px-[15px] py-2.5 text-[13px]"
      style="box-shadow: var(--shadow); border-left: 3px solid {edgeColor(t.cls)};"
    >
      {t.msg}
    </div>
  {/each}
</div>
