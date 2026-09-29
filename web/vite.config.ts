import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

// 构建输出到 Go 的嵌入目录（internal/panel/dist），go build 时经 go:embed 打进二进制。
// dist 不进版本库（只有 .gitkeep，emptyOutDir 会清掉它，postbuild 再补回来）；
// Docker 镜像与 CI 发布的二进制会先构建前端，本地要先 npm run build 再 go build。
export default defineConfig({
  base: '/panel/',
  plugins: [react(), tailwindcss()],
  build: {
    outDir: '../internal/panel/dist',
    emptyOutDir: true,
    chunkSizeWarningLimit: 1500,
    rolldownOptions: {
      output: {
        // 体积大、很少变的第三方库各自成包：只改页面代码时这些文件名（内容哈希）不变，
        // 提交进仓库的产物 diff 只落在很小的业务包上。Recharts 不单独分组：只有两个按需加载的图表页用它，
        // 交给打包器自动拆（手动分组会把它和主包共用的小库绑在一起，拖进首屏）
        codeSplitting: {
          groups: [
            { name: 'heroui', test: /node_modules[\\/](@heroui|react-aria|react-aria-components|react-stately|@react-aria|@react-stately|@react-types|@internationalized|@swc|tailwind-variants|tailwind-merge|input-otp|@gravity-ui)[\\/]/ },
            { name: 'react', test: /node_modules[\\/](react|react-dom|scheduler|@tanstack)[\\/]/ },
          ],
        },
      },
    },
  },
  server: {
    // 开发时把面板接口转发到本机网关（默认监听 :7863）
    proxy: { '/panel/api': 'http://127.0.0.1:7863' },
  },
  test: {
    include: ['src/**/*.test.ts'],
    environment: 'node',
  },
})
