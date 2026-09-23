<script>
  let { open = false, title = '', hint = '', wide = false, onclose = null, children, footer } = $props();

  function onkey(e) {
    if (e.key === 'Escape') onclose?.();
  }
</script>

<svelte:window onkeydown={onkey} />

{#if open}
  <!-- svelte-ignore a11y_click_events_have_key_events a11y_no_static_element_interactions -->
  <div
    class="fixed inset-0 z-[60] flex items-center justify-center p-5"
    style="background: rgba(6, 8, 13, 0.62); backdrop-filter: blur(2px);"
    onclick={(e) => {
      if (e.target === e.currentTarget) onclose?.();
    }}
  >
    <div
      class="flex max-h-[86vh] flex-col rounded-[13px] border border-line bg-surface {wide
        ? 'w-[940px]'
        : 'w-[560px]'} max-w-full"
      style="box-shadow: 0 24px 64px -24px rgba(0, 0, 0, 0.6);"
      role="dialog"
      aria-modal="true"
      aria-label={title}
    >
      <header class="border-b border-linesoft px-5 pb-3 pt-4">
        <h3 class="text-[15px] font-semibold">{title}</h3>
        {#if hint}
          <div class="mt-1 text-[12.5px] text-ink3">{hint}</div>
        {/if}
      </header>
      <div class="overflow-auto px-5 py-[18px]">
        {@render children?.()}
      </div>
      {#if footer}
        <footer class="flex gap-2.5 border-t border-linesoft px-5 py-3">
          {@render footer()}
        </footer>
      {/if}
    </div>
  </div>
{/if}
