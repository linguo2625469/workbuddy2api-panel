import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider } from '@tanstack/react-router'
import './index.css'
import { queryClient } from './lib/queries'
import { router } from './router'

// 发了新版本后，旧标签页去加载已经不存在的分包会失败：刷新一下拿到新版本
window.addEventListener('vite:preloadError', (e) => {
  e.preventDefault()
  location.reload()
})

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  </StrictMode>,
)
