import { spawn } from "node:child_process";
import { existsSync, mkdirSync, readFileSync } from "node:fs";
import { resolve } from "node:path";
import { parseEnv } from "node:util";

const root = resolve(import.meta.dirname, "..");
const file = resolve(root, ".env");
const env = { ...process.env, ...(existsSync(file) ? parseEnv(readFileSync(file, "utf8")) : {}) };
env.DEVELOPMENT = "true";
env.BASE_URL = "http://localhost:8080";
env.PORT = "8080";
env.ALLOWED_HOSTS = "localhost,127.0.0.1";
env.DATA_DIR = resolve(root, ".local-data");
env.ASSETS_DIR = resolve(root, "web/dist");
env.TRUSTED_PROXIES = "";
env.PROXY_BUDGET_START_DATE ||= new Date().toISOString().slice(0, 10);
env.TURNSTILE_SITE_KEY ||= "1x00000000000000000000BB"; // Cloudflare's always-pass test sitekey
mkdirSync(env.DATA_DIR, { recursive: true });

const build = spawn("pnpm", ["run", "build"], { cwd: root, env, stdio: "inherit" });
build.on("error", error => { console.error(error.message); process.exitCode = 1; });
build.on("exit", code => {
  if (code !== 0) { process.exitCode = code || 1; return; }
  // `go run` does not forward signals to the compiled server, so signal its process group.
  const server = spawn("go", ["run", "."], { cwd: resolve(root, "server"), env, stdio: "inherit", detached: true });
  server.on("error", error => { console.error(error.message); process.exitCode = 1; });
  server.on("exit", code => { process.exitCode = code || 0; });
  for (const signal of ["SIGINT", "SIGTERM", "SIGHUP"]) {
    process.on(signal, () => { try { process.kill(-server.pid, signal); } catch { /* already gone */ } });
  }
});
