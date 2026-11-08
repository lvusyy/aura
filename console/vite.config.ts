import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// aura-console 构建配置：产物 embed 进 aura-controller 单二进制，dev 联调走 proxy 转发 /aura.v1 到本地 controller。
export default defineConfig({
  // SPA 部署于 controller 的 /console/* 前缀（console-design §2），资源引用绝对路径防深链回退破相对路径。
  base: "/console/",
  plugins: [react()],
  build: {
    // embed 目标：controller/internal/console/dist（TASK-003 //go:embed all:dist 消费）。
    outDir: "../controller/internal/console/dist",
    // outDir 在工程根之外，显式允许清空。
    emptyOutDir: true,
    chunkSizeWarningLimit: 1200,
    rollupOptions: {
      output: {
        manualChunks(id) {
          if (id.includes("node_modules")) {
            if (id.includes("@ant-design/icons")) {
              return "vendor-icons";
            }
            if (id.includes("antd") || id.includes("@ant-design") || id.includes("rc-")) {
              return "vendor-antd";
            }
            if (id.includes("@connectrpc") || id.includes("@bufbuild")) {
              return "vendor-rpc";
            }
            if (id.includes("@yume-chan") || id.includes("scrcpy")) {
              return "vendor-tango";
            }
            return "vendor-core";
          }
        },
      },
    },
  },
  server: {
    proxy: {
      // dev 联调后端 controller（自签 TLS，secure:false 放行）；connect-web 打 /aura.v1.* 前缀。
      "/aura.v1": {
        target: "https://127.0.0.1:18080",
        changeOrigin: true,
        secure: false,
      },
    },
  },
});
