// 复制文本：clipboard API 只在安全上下文（https / localhost）可用，
// 远程 http 访问面板时拿不到 navigator.clipboard，退回 execCommand。
export function copyText(text: string): Promise<void> {
  if (navigator.clipboard && window.isSecureContext) return navigator.clipboard.writeText(text)
  return new Promise((resolve, reject) => {
    const ta = document.createElement('textarea')
    ta.value = text
    ta.style.cssText = 'position:fixed;opacity:0'
    document.body.appendChild(ta)
    ta.select()
    try {
      if (document.execCommand('copy')) resolve()
      else reject(new Error('copy failed'))
    } catch (e) {
      reject(e instanceof Error ? e : new Error(String(e)))
    } finally {
      ta.remove()
    }
  })
}
