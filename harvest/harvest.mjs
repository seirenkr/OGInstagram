#!/usr/bin/env node
// Harvest an external-helper HMAC signing key using Obscura (headless V8) via Playwright CDP.
// The signer is standard HMAC-SHA256; only the raw key + fixed _ts are build-specific.
// We run the real signer inside a real V8 (passes the anti-tamper gate), hook crypto.subtle.importKey
// to capture the raw key bytes, and read the fixed _ts from a controlled sign call.
//
// Usage: node harvest.mjs [ws://127.0.0.1:9222] <site-url>
// Prints JSON: { key, fixedTs }

import { chromium } from "playwright-core";

const CDP = process.argv[2] || "ws://127.0.0.1:9222";
const SITE = process.argv[3];
if (!SITE) throw new Error("external-helper site URL is required");
const log = (...a) => console.error("[harvest]", ...a);

const browser = await chromium.connectOverCDP(CDP);
try {
  const context = browser.contexts()[0] || await browser.newContext();
  const page = await context.newPage();

  log("navigating", SITE);
  await page.goto(SITE, { waitUntil: "domcontentloaded", timeout: 45000 });
  await page.waitForTimeout(1200); // let app.js mount the webpack runtime

  // Hijack the webpack runtime to obtain __webpack_require__, then force-load the lazy
  // signer chunk (id 54 → js/link.chunk.js) directly, without driving the UI.
  log("hijacking webpack runtime + loading signer chunk (54)…");
  const loaded = await page.evaluate(async () => {
    await new Promise((res) => {
      window.webpackChunk.push([["__harvest"], {}, (require) => { window.__wpreq = require; res(); }]);
      setTimeout(res, 4000);
    });
    const req = window.__wpreq;
    if (!req || typeof req.e !== "function") return { ok: false, why: "no require.e" };
    await req.e(54);
    return { ok: !!(req.m && req.m["27"]), nmods: req.m ? Object.keys(req.m).length : 0 };
  });
  if (!loaded.ok) throw new Error("failed to load signer module: " + JSON.stringify(loaded));

  // Hook crypto.subtle.importKey, then re-run module 27's factory so the fresh signer
  // binds our hooked subtle. The importKey call receives the raw HMAC key bytes.
  log("hooking subtle + re-running signer module…");
  const result = await page.evaluate(async () => {
    const r = window.__wpreq;
    const keys = [];
    const realImport = crypto.subtle.importKey.bind(crypto.subtle);
    crypto.subtle.importKey = async function (fmt, keyData, algo, ext, usages) {
      try {
        const b = keyData instanceof ArrayBuffer ? new Uint8Array(keyData)
          : ArrayBuffer.isView(keyData) ? new Uint8Array(keyData.buffer, keyData.byteOffset, keyData.byteLength) : null;
        if (b) keys.push({ hex: [...b].map(x => x.toString(16).padStart(2, "0")).join(""), algo: JSON.stringify(algo) });
      } catch (e) { keys.push({ err: String(e) }); }
      return realImport(fmt, keyData, algo, ext, usages);
    };
    // Re-execute the module 27 factory with a fresh module object → re-binds hooked subtle.
    const mm = { id: 27, exports: {}, loaded: false };
    r.m["27"](mm, mm.exports, r);
    const fn = await mm.exports.default; // gate passes inside real V8
    const out = await fn({ username: "HELLO", maxId: "MX" });
    return { keys, out };
  });

  const keyHit = (result.keys || []).find(k => k.hex && /^[0-9a-f]{64}$/.test(k.hex));
  // Stderr reaches the server log: report candidate key lengths, never key bytes.
  if (!keyHit) throw new Error("HMAC key not captured: " + JSON.stringify((result.keys || []).map(k => k.hex ? k.hex.length / 2 : k.err)));
  const key = keyHit.hex;
  const fixedTs = result.out._ts;

  // Capture only — no live verification here. The external helper's Cloudflare
  // captcha-challenges Node/undici's TLS fingerprint (422) before the signature
  // is ever checked, so a Node probe is meaningless in-container. The caller (Go
  // server) installs the key directly; a wrong key self-corrects on the next 401.
  console.log(JSON.stringify({ key, fixedTs }));
} finally {
  await browser.close();
}
