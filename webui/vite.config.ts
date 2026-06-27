import path from "path"
import tailwindcss from "@tailwindcss/vite"
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react-swc'

// https://vite.dev/config/
export default defineConfig({
    plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      "@": path.resolve(__dirname, "./src"),
    },

  },
  // 禁用 esbuild（其 service 在部分环境下会 EPIPE），JSX 已由 @vitejs/plugin-react-swc 处理，
  // 最终压缩改用 terser。
  build: {
    chunkSizeWarningLimit: 1000,
    minify: "terser",
    rollupOptions: {
      output: {
        manualChunks: {
          // React 生态系统
          'react-vendor': ['react', 'react-dom', 'react-router-dom'],

          // UI 组件库
          'ui-vendor': [
            '@radix-ui/react-alert-dialog',
            '@radix-ui/react-checkbox',
            '@radix-ui/react-dialog',
            '@radix-ui/react-label',
            '@radix-ui/react-radio-group',
            '@radix-ui/react-select',
            '@radix-ui/react-slot',
            '@radix-ui/react-switch',
            '@radix-ui/react-tooltip'
          ],

          // 图表库
          'charts-vendor': ['recharts', 'lucide-react'],

          // 表单和验证
          'form-vendor': ['react-hook-form', '@hookform/resolvers', 'zod'],

          // 工具库
          'utils-vendor': ['clsx', 'tailwind-merge', 'class-variance-authority']
        }
      }
    }
  },
  server: {
    proxy: {
      '/api': {
        target: 'http://localhost:7070',
        changeOrigin: true,
        secure: false,
      }
    }
  }
})
