import { defineConfig } from 'vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';
import tailwindcss from '@tailwindcss/vite';

// base 固定 /panel/：网关把面板挂在同源 /panel/ 前缀下，构建产物经 go:embed
// 进二进制，运行时无外部依赖。dev 下 /panel/api 代理到本地网关。
export default defineConfig({
  base: '/panel/',
  plugins: [svelte(), tailwindcss()],
  build: { outDir: '../dist', emptyOutDir: true, assetsDir: 'assets' },
  server: { proxy: { '/panel/api': 'http://127.0.0.1:7863' } }
});
