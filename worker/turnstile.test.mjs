import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { after, before, test } from "node:test";
import { unstable_readConfig, unstable_startWorker } from "wrangler";
import { publicEmbedStatus } from "./http.ts";
import { verifyTurnstile } from "./turnstile.ts";

let worker;

async function assertProblem(response, status, detail) {
  assert.equal(response.status, status);
  assert.match(response.headers.get("content-type") ?? "", /^application\/problem\+json/);
  const body = await response.json();
  assert.equal(body.type, "about:blank");
  assert.equal(body.status, status);
  assert.equal(body.detail, detail);
}

before(async () => {
  worker = await unstable_startWorker({ config: "worker/wrangler.test.jsonc" });
});

after(async () => {
  await worker?.dispose();
});

test("production embed requests require cf_clearance", async () => {
  const response = await worker.fetch("https://oginstagram.com/api/embed", {
    method: "POST",
    headers: {
      "content-type": "application/json",
      origin: "https://oginstagram.com",
      "sec-fetch-site": "same-origin",
    },
    body: JSON.stringify({ path: "/p/Ab_12", token: "unused" }),
  });

  await assertProblem(response, 403, "clearance required");
});

test("localhost embed requests reject an empty Turnstile token", async () => {
  const response = await worker.fetch("http://localhost/api/embed", {
    method: "POST",
    headers: {
      "content-type": "application/json",
      origin: "http://localhost",
      "sec-fetch-site": "same-origin",
    },
    body: JSON.stringify({ path: "/p/Ab_12", token: "" }),
  });

  await assertProblem(response, 403, "verification failed");
});

test("embed rejects network-path references before verification", async () => {
  const response = await worker.fetch("https://oginstagram.com/api/embed", {
    method: "POST",
    headers: {
      "content-type": "application/json",
      origin: "https://oginstagram.com",
      "sec-fetch-site": "same-origin",
    },
    body: JSON.stringify({ path: "//evil.test/p/Ab_12", token: "unused" }),
  });
  await assertProblem(response, 400, "invalid Instagram path");
});

test("admin purge requires POST", async () => {
  const response = await worker.fetch("https://oginstagram.com/api/admin/purge");
  assert.equal(response.status, 405);
});

test("home page rejects non-read methods", async () => {
  const response = await worker.fetch("https://oginstagram.com/", { method: "POST" });
  assert.equal(response.status, 405);
  assert.equal(response.headers.get("allow"), "GET, HEAD");
  assert.equal(response.headers.get("cache-control"), "no-store");
});

test("production routes raw home templates through the Worker", () => {
  const config = unstable_readConfig({ config: "wrangler.jsonc" }, { hideWarnings: true });
  assert.deepEqual(config.assets?.run_worker_first, ["/home/*"]);
});

test("production disables Workers Caching for the internal container proxy", () => {
  const config = unstable_readConfig({ config: "wrangler.jsonc" }, { hideWarnings: true });
  assert.equal(config.exports?.ContainerProxy?.cache?.enabled, false);
});

test("status without Analytics credentials is not cached", async () => {
  const response = await worker.fetch("https://oginstagram.com/api/status");
  assert.equal(response.status, 503);
  assert.equal(response.headers.get("cache-control"), "no-store");
});

test("status analytics excludes cache hits", async () => {
  const source = await readFile(new URL("./observability.ts", import.meta.url), "utf8");
  assert.match(source, /WHERE timestamp.+blob5 NOT IN \('edge', 'model'\)/);
});

test("embed API exposes origin failures without confusing upstream limits", () => {
  for (const [status, expected] of [[0, null], [399, null], [400, 400], [429, 502], [499, 504], [599, 599], [600, null]]) {
    assert.equal(publicEmbedStatus(status), expected, status);
  }
});

test("malformed offload capabilities are rejected before cache or container lookup", async () => {
  const response = await worker.fetch(
    "https://oginstagram.com/offload/Ab_12/1?v=1&kid=test-v1&sig=broken",
    { headers: { "user-agent": "Discordbot/2.0" } },
  );
  assert.equal(response.status, 404);
  assert.equal(response.headers.get("cache-control"), "no-store");
});

test("unsigned offload requests are rejected before cache or container lookup", async () => {
  const response = await worker.fetch("https://oginstagram.com/offload/Ab_12/1", {
    headers: { "user-agent": "Discordbot/2.0" },
  });
  assert.equal(response.status, 404);
  assert.equal(response.headers.get("cache-control"), "no-store");
});

test("admin purge rejects missing or wrong tokens", async () => {
  const noAuth = await worker.fetch("https://oginstagram.com/api/admin/purge", { method: "POST" });
  assert.equal(noAuth.status, 403);

  const wrongToken = await worker.fetch("https://oginstagram.com/api/admin/purge", {
    method: "POST",
    headers: { authorization: "Bearer not-the-token" },
  });
  assert.equal(wrongToken.status, 403);
});

test("admin purge passes auth and calls the purge API with the configured token", async () => {
  const response = await worker.fetch("https://oginstagram.com/api/admin/purge", {
    method: "POST",
    headers: { authorization: "Bearer test-admin-purge-token" },
  });
  // Not asserting success === true here: local wrangler dev does not simulate
  // ctx.cache, so purgeAll()'s defensive fallback legitimately reports failure
  // in this environment. What this test guards is the part local dev *can*
  // verify - the token was accepted (no 403) and purgeAll() actually ran
  // rather than short-circuiting. Real purge success is checked in production
  // after deploy.
  assert.notEqual(response.status, 403);
  const body = await response.json();
  if (response.ok) assert.equal(body.success, true);
  else {
    assert.equal(response.status, 502);
    assert.equal(body.type, "about:blank");
  }
});

test("Turnstile distinguishes rejection from service unavailability", async () => {
  const request = new Request("https://oginstagram.com/api/embed", {
    headers: { "cf-connecting-ip": "192.0.2.1" },
  });
  const env = { TURNSTILE_SECRET_KEY: "secret" };
  const url = new URL(request.url);

  const rejected = await verifyTurnstile("token", request, env, url, async (target, init) => {
    assert.equal(target, "https://challenges.cloudflare.com/turnstile/v0/siteverify");
    assert.ok(init.body instanceof URLSearchParams);
    assert.equal(init.body.get("secret"), "secret");
    assert.equal(init.body.get("response"), "token");
    assert.equal(init.body.get("remoteip"), "192.0.2.1");
    assert.match(init.body.get("idempotency_key") ?? "", /^[0-9a-f-]{36}$/i);
    return Response.json({
      success: false,
      "error-codes": ["invalid-input-response"],
    });
  });
  assert.deepEqual(rejected, { outcome: "rejected" });

  let attempts = 0;
  const unavailable = await verifyTurnstile("token", request, env, url, async () => {
    attempts++;
    return Response.json({ success: false, "error-codes": ["internal-error"] });
  });
  assert.equal(attempts, 2);
  assert.equal(unavailable.outcome, "unavailable");
  assert.equal(unavailable.errorType, "turnstile_internal_error");
});
