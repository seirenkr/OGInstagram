import { readdirSync, readFileSync, renameSync } from "node:fs";
import { createHash } from "node:crypto";
import { fileURLToPath } from "node:url";

const dir = fileURLToPath(new URL("./dist/", import.meta.url));
for (const name of readdirSync(dir)) {
  const m = /^favicon-(\d+)\.png$/.exec(name);
  if (!m) continue;
  const hash = createHash("sha256").update(readFileSync(dir + name)).digest("hex").slice(0, 8);
  const next = `favicon-${m[1]}-${hash}.png`;
  renameSync(dir + name, dir + next);
  console.log(`favicon: ${name} -> ${next}`);
}
