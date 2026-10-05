import { copyFileSync, lstatSync, readFileSync, readdirSync, rmSync, statSync, writeFileSync, mkdirSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const BRAND = "OGInstagram";
const BRAND_COLOR = "#ff0069";
const SUPPORT_URL = "https://ko-fi.com/seirenkr";
const GITHUB_URL = "https://github.com/seirenkr/OGInstagram";
const TWEMOJI_VERSION = "15.0.0";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const dist = join(root, "web", "dist");
const template = readFileSync(join(dist, "index.html"), "utf8");
const localeDir = join(root, "web", "locales");
const twemojiSource = join(root, "node_modules", "@twemoji", "svg");
const twemojiPackage = JSON.parse(readFileSync(join(twemojiSource, "package.json"), "utf8"));
if (twemojiPackage.version !== TWEMOJI_VERSION) {
  throw new Error(`Twemoji asset version mismatch: expected ${TWEMOJI_VERSION}, found ${twemojiPackage.version}`);
}
const twemojiFiles = readdirSync(twemojiSource)
  .filter((file) => /^[0-9a-f]+(?:-[0-9a-f]+)*\.svg$/.test(file))
  .sort();
if (twemojiFiles.length < 3000) throw new Error("Twemoji SVG assets are incomplete");
const previewSource = readFileSync(join(root, "web", "src", "preview.tsx"), "utf8");
if (!previewSource.includes(`const TWEMOJI_BASE = "/twemoji/v${TWEMOJI_VERSION}/";`)) {
  throw new Error("Twemoji browser asset URL does not match the packaged version");
}
const locales = readdirSync(localeDir).filter((f) => f.endsWith(".json")).map((f) => f.slice(0, -5)).sort();
if (!locales.includes("en")) throw new Error("en locale missing");

const escapeHTML = (s) => s.replaceAll("&", "&amp;").replaceAll('"', "&quot;").replaceAll("<", "&lt;").replaceAll(">", "&gt;");

const strings = new Map(locales.map((l) => [l, JSON.parse(readFileSync(join(localeDir, `${l}.json`), "utf8"))]));

const requireComplete = (locale, node, path = "") => {
  for (const [key, value] of Object.entries(node)) {
    const at = path ? `${path}.${key}` : key;
    if (typeof value === "string") {
      if (!value) throw new Error(`locale ${locale}: ${at} is empty`);
    } else if (value && typeof value === "object") requireComplete(locale, value, at);
  }
};
const keyShape = (node) => Object.entries(node).map(([k, v]) => (v && typeof v === "object" ? `${k}{${keyShape(v)}}` : k)).sort().join(",");
for (const [locale, t] of strings) {
  requireComplete(locale, t);
  if (keyShape(t) !== keyShape(strings.get("en"))) throw new Error(`locale ${locale}: keys differ from en`);
}

if (!template.includes('<div id="root"></div>')) throw new Error("root container missing from web/index.html");

const languageTag = (locale) => locale === "zh-hans" ? "zh-Hans" : locale === "zh-hant" ? "zh-Hant" : locale;

mkdirSync(join(dist, "home"), { recursive: true });
const twemojiDestination = join(dist, "twemoji", `v${TWEMOJI_VERSION}`);
rmSync(join(dist, "twemoji"), { recursive: true, force: true });
mkdirSync(twemojiDestination, { recursive: true });
let twemojiBytes = 0;
for (const file of twemojiFiles) {
  const source = join(twemojiSource, file);
  const destination = join(twemojiDestination, file);
  copyFileSync(source, destination);
  if (lstatSync(destination).isSymbolicLink()) throw new Error(`Twemoji asset remained a symlink: ${file}`);
  twemojiBytes += statSync(destination).size;
}
for (const locale of locales) {
  const t = strings.get(locale);
  const lang = languageTag(locale);
  const appJSON = JSON.stringify({
    brand: BRAND,
    version: "__OG_VERSION__",
    host: "__OG_HOST__",
    supportUrl: SUPPORT_URL,
    githubUrl: GITHUB_URL,
    turnstileSiteKey: "__OG_TURNSTILE_SITE_KEY__",
    ...t,
    lang,
  }).replaceAll("<", "\\u003c");
  const headLinks = [
    `<link rel="canonical" href="__OG_CANONICAL__">`,
    `<meta name="theme-color" content="${BRAND_COLOR}">`,
    `<meta property="og:url" content="__OG_CANONICAL__">`,
    `<meta property="og:image" content="__OG_BASE__/favicon-192.png">`,
    `<meta name="twitter:image" content="__OG_BASE__/favicon-192.png">`,
    ...locales.map((l) => `<link rel="alternate" hreflang="${languageTag(l)}" href="__OG_BASE__/?hl=${l}">`),
    `<link rel="alternate" hreflang="x-default" href="__OG_BASE__/">`,
  ].join("");
  const html = template
    .replaceAll("{{BRAND}}", escapeHTML(BRAND))
    .replaceAll("{{LANG}}", lang)
    .replaceAll("{{T_TAGLINE}}", escapeHTML(t.tagline))
    .replace('<meta name="og-head-links-placeholder">', headLinks)
    .replaceAll("{{APP_JSON}}", appJSON);
  if (html.includes("{{")) throw new Error(`unreplaced placeholder in ${locale} home HTML`);
  writeFileSync(join(dist, "home", `${locale}.html`), html);
}
rmSync(join(dist, "index.html"));
console.log(`built ${locales.length} home pages and ${twemojiFiles.length} local Twemoji SVGs (${(twemojiBytes / 1024 / 1024).toFixed(1)} MiB)`);
