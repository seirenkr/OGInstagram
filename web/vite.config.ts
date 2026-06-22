import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import { resolve } from "node:path";

export default defineConfig({
  root: __dirname,
  plugins: [react()],
  define: { "process.env.NODE_ENV": JSON.stringify("production") },
  build: {
    outDir: "dist",
    emptyOutDir: true,
    cssCodeSplit: false,
    lib: { entry: resolve(__dirname, "src/main.tsx"), formats: ["es"], fileName: () => "main.js" },
    rollupOptions: {
      output: { assetFileNames: asset => (asset.name?.endsWith(".css") ? "main.css" : "assets/[name]-[hash][extname]") },
    },
  },
});
