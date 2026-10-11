import { readFileSync, readdirSync, rmSync, writeFileSync, mkdirSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const BRAND = "OGInstagram";
const BRAND_COLOR = "#ff0069";
const SUPPORT_URL = "https://ko-fi.com/seirenkr";
const GITHUB_URL = "https://github.com/seirenkr/OGInstagram";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const dist = join(root, "web", "dist");
const template = readFileSync(join(dist, "index.html"), "utf8");
const localeDir = join(root, "web", "locales");
const locales = readdirSync(localeDir).filter((f) => f.endsWith(".json")).map((f) => f.slice(0, -5)).sort();
if (!locales.includes("en")) throw new Error("en locale missing");

const escapeHTML = (s) => s.replaceAll("&", "&amp;").replaceAll('"', "&quot;").replaceAll("<", "&lt;").replaceAll(">", "&gt;");

const strings = new Map(locales.map((l) => [l, JSON.parse(readFileSync(join(localeDir, `${l}.json`), "utf8"))]));

// Throws on an empty leaf while building a sorted key signature of the tree.
const keyShape = (locale, node, path = "") => Object.entries(node).map(([key, value]) => {
  const at = path ? `${path}.${key}` : key;
  if (value && typeof value === "object") return `${key}{${keyShape(locale, value, at)}}`;
  if (!value) throw new Error(`locale ${locale}: ${at} is empty`);
  return key;
}).sort().join(",");
const enShape = keyShape("en", strings.get("en"));
for (const [locale, t] of strings) {
  if (keyShape(locale, t) !== enShape) throw new Error(`locale ${locale}: keys differ from en`);
  if (!t.hero.line2.includes("{rich}") || !t.js.timeUTC.includes("UTC")) throw new Error(`locale ${locale}: hero.line2 needs {rich} and js.timeUTC needs UTC`);
}

if (!template.includes('<div id="root"></div>')) throw new Error("root container missing from web/index.html");
if (!template.includes('<meta name="og-head-links-placeholder">')) throw new Error("head-links placeholder missing from web/index.html");

const languageTag = (locale) => locale === "zh-hans" ? "zh-Hans" : locale === "zh-hant" ? "zh-Hant" : locale;

mkdirSync(join(dist, "home"), { recursive: true });
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
    .replaceAll("{{T_TAGLINE}}", () => escapeHTML(t.tagline))
    .replace('<meta name="og-head-links-placeholder">', () => headLinks)
    .replaceAll("{{APP_JSON}}", () => appJSON);
  if (html.includes("{{")) throw new Error(`unreplaced placeholder in ${locale} home HTML`);
  writeFileSync(join(dist, "home", `${locale}.html`), html);
}
rmSync(join(dist, "index.html"));
console.log(`built ${locales.length} home pages`);
