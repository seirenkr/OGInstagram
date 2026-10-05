# Turnstile Pre-clearance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Execute Turnstile only after Cloudflare returns `Cf-Mitigated: challenge`, validate that one generated token, and reuse WAF-validated `cf_clearance` for later preview requests.

**Architecture:** `POST /api/embed` remains the only application endpoint. The browser first posts without a token, reacts only to Cloudflare's challenge header, executes the existing pre-clearance widget, and retries once with the token. The Worker validates a present token using the canonical Spin Siteverify call; an absent token relies on the exact Managed Challenge WAF boundary and the existing clearance-cookie rate key.

**Tech Stack:** React 19, TypeScript 5.9, Cloudflare Workers, Turnstile, Node's built-in test runner, Wrangler.

## Global Constraints

- Production WAF expression: `http.request.method eq "POST" and http.request.uri.path eq "/api/embed"`; action: Managed Challenge.
- Turnstile widget hostname: `oginstagram.com`; pre-clearance level: `managed`.
- Detect a challenge only with `response.headers.get("cf-mitigated") === "challenge"`.
- Retry the protected request at most once and reset the widget only after that request completes.
- Every generated token must be sent to Siteverify exactly once; no token is generated for a clearance reuse request.
- Turnstile-related rejection and Siteverify failure response: plain-text `forbidden`, HTTP 403.
- No application session endpoint, custom cookie, dependency, or unrelated refactor.
- Preserve all pre-existing dirty-worktree changes; do not create implementation commits that would capture unrelated user work.

## File map

- `worker/turnstile.ts`: canonical boolean Siteverify validator.
- `worker/preview.ts`: optional token contract and WAF/pre-clearance trust boundary.
- `worker/turnstile.test.mjs`: Worker behavior, Siteverify request, and frontend request-boundary checks.
- `web/src/preview-request.ts`: testable `/api/embed` transport and Cloudflare challenge detector.
- `web/src/preview.tsx`: first request, challenge detection, one widget execution, one retry, and post-request reset.
- `wrangler.jsonc`: production `TURNSTILE_HOSTNAMES` non-secret variable.
- `worker/wrangler.test.jsonc`: test hostname variable.
- `.dev.vars.example`: localhost hostname variable.
- `tsconfig.json`: allow explicit TypeScript import extensions used by the direct Node behavior test.
- `worker-configuration.d.ts`: Wrangler-generated environment type.
- `README.md`: exact WAF rule, Challenge Passage behavior, and token/clearance request flow.

---

### Task 1: Worker clearance reuse and canonical Siteverify

**Files:**
- Modify: `worker/turnstile.test.mjs`
- Modify: `worker/turnstile.ts`
- Modify: `worker/preview.ts`
- Modify: `wrangler.jsonc`
- Modify: `worker/wrangler.test.jsonc`
- Modify: `.dev.vars.example`
- Modify: `tsconfig.json`
- Regenerate: `worker-configuration.d.ts`

**Interfaces:**
- Consumes: `serveEmbed(request, env, ctx, url, requestId)` and the existing `cf_clearance` digest rate key.
- Produces: `verifyTurnstile(token, request, env, fetcher?) => Promise<boolean>` and JSON body `{ path: string; token?: string }`.

- [ ] **Step 1: Write failing Worker tests**

Add an `assertForbidden` helper that requires status 403, body exactly `forbidden`, and a `text/plain` content type. Change the missing-clearance and empty-token expectations to this helper. Add a direct `serveEmbed` test using a fake `cf_clearance`, a successful fake rate limiter, and a stub `ctx.exports.Embed.fetch` returning `new Response("preview")`; post only `{ path: "/p/Ab_12" }` and require the response body `preview`.

Replace the outcome-object test with a canonical boolean validation test:

```js
const valid = await verifyTurnstile("token", request, {
  TURNSTILE_SECRET_KEY: "secret",
  TURNSTILE_HOSTNAMES: "oginstagram.com",
}, async (target, init) => {
  assert.equal(target, "https://challenges.cloudflare.com/turnstile/v0/siteverify");
  assert.equal(init.headers["Content-Type"], "application/x-www-form-urlencoded");
  assert.equal(init.body.get("secret"), "secret");
  assert.equal(init.body.get("response"), "token");
  assert.equal(init.body.get("remoteip"), "192.0.2.1");
  return Response.json({ success: true, action: "turnstile-spin-v1", hostname: "oginstagram.com" });
});
assert.equal(valid, true);
```

Require false for an invalid action, invalid hostname, empty or over-2048-byte token, missing hostname configuration, non-2xx Siteverify response, and thrown fetch error.

- [ ] **Step 2: Run the focused test and confirm RED**

Run:

```bash
node --experimental-strip-types --test worker/turnstile.test.mjs
```

Expected: failures because missing clearance still returns a problem document, token is required, `TURNSTILE_HOSTNAMES` is unused, and `verifyTurnstile` returns an outcome object.

- [ ] **Step 3: Implement the minimum Worker change**

In `worker/turnstile.ts`, parse `env.TURNSTILE_HOSTNAMES` into a non-empty set and return `false` for invalid configuration/token. Make one canonical request:

```ts
const response = await fetcher("https://challenges.cloudflare.com/turnstile/v0/siteverify", {
  method: "POST",
  headers: { "Content-Type": "application/x-www-form-urlencoded" },
  signal: AbortSignal.timeout(10_000),
  body: new URLSearchParams({
    secret: env.TURNSTILE_SECRET_KEY,
    response: token,
    remoteip: request.headers.get("CF-Connecting-IP") ?? "",
  }),
});
```

Return `true` only for a 2xx JSON result with `success === true`, action `turnstile-spin-v1`, and a hostname in the configured set; catch every HTTP/JSON/network error and return `false`. Remove the custom retry, idempotency key, availability outcome, and diagnostic response branching.

In `worker/preview.ts`, parse `{ path, token? }`. Require a valid production clearance rate key, returning `new Response("forbidden", { status: 403 })` when missing. Preserve the existing localhost/127.0.0.1 development rate-key fallback because `wrangler dev` has no challenge platform. Call Siteverify only when `token !== undefined`; return the same response when it returns false.

Set `TURNSTILE_HOSTNAMES` to `oginstagram.com` in `wrangler.jsonc`, and to `localhost,127.0.0.1` in test/dev configuration. Enable `allowImportingTsExtensions` in the root `tsconfig.json` so the direct Node test can import the real handler without weakening the standard type check. Run `wrangler types` to update `worker-configuration.d.ts`.

- [ ] **Step 4: Run the focused test and confirm GREEN**

Run:

```bash
node --experimental-strip-types --test worker/turnstile.test.mjs
```

Expected: all tests pass.

---

### Task 2: Reactive frontend challenge and one retry

**Files:**
- Modify: `worker/turnstile.test.mjs`
- Create: `web/src/preview-request.ts`
- Modify: `web/src/preview.tsx`

**Interfaces:**
- Consumes: `/api/embed` body `{ path: string; token?: string }` and Cloudflare response header `Cf-Mitigated: challenge`.
- Produces: `requestPreview(path, token?, signal, fetcher?) => Promise<Response>`, `fetchPreview(path, token?, signal, serviceHost, dateFormatter)`, and `runPreview(path, token?) => Promise<void>`.

- [ ] **Step 1: Write failing request-boundary tests**

Import the not-yet-created `requestPreview` from `web/src/preview-request.ts`. With a fake fetcher at the external HTTP boundary, assert the actual serialized request and returned behavior:

```js
await assert.rejects(
  requestPreview("/p/Ab_12", undefined, new AbortController().signal, async (target, init) => {
    assert.equal(target, "/api/embed");
    assert.deepEqual(JSON.parse(init.body), { path: "/p/Ab_12" });
    return new Response("challenge", { headers: { "cf-mitigated": "challenge" } });
  }),
  { message: "cloudflare-challenge" },
);

const response = await requestPreview("/p/Ab_12", "token", new AbortController().signal, async (_target, init) => {
  assert.deepEqual(JSON.parse(init.body), { path: "/p/Ab_12", token: "token" });
  return new Response("preview", { status: 403 });
});
assert.equal(response.status, 403);
```

The second assertion proves status 403 alone is not treated as a Cloudflare challenge.

- [ ] **Step 2: Run the focused test and confirm RED**

Run:

```bash
node --experimental-strip-types --test worker/turnstile.test.mjs
```

Expected: the test fails because the request-boundary module does not exist.

- [ ] **Step 3: Implement the minimum browser state transition**

Create `web/src/preview-request.ts`. Make `token` optional and omit the property from the first JSON request. Immediately after `fetch`, before returning the response, throw `new Error("cloudflare-challenge")` only when:

```ts
response.headers.get("cf-mitigated") === "challenge"
```

Use `requestPreview` inside the existing HTML-parsing `fetchPreview`. Make `runPreview(path, token?)` return its promise. If the sentinel is caught without a token, retain `path` in `pendingPath`, set `verifying`, load Turnstile, and execute it. If it is caught after a token retry, show the existing generic fetch error; never execute a second challenge.

Submit valid input with `void runPreview(path)` directly. In the Turnstile success callback, call `runPreview(path, token)` and use its `finally` block to clear `pendingPath`, reset the widget, and clear `verifying`. Keep the existing error, expiry, and timeout callbacks routed through `failVerification`.

- [ ] **Step 4: Run frontend checks and confirm GREEN**

Run:

```bash
node --experimental-strip-types --test worker/turnstile.test.mjs
pnpm exec eslint web/src/preview.tsx
pnpm exec tsc -p web/tsconfig.json --noEmit
```

Expected: all commands exit 0.

---

### Task 3: Deployment contract and complete verification

**Files:**
- Modify: `README.md`

**Interfaces:**
- Consumes: implemented Worker/browser behavior and the exact production WAF expression.
- Produces: deployment instructions sufficient to make the Worker no-token path secure.

- [ ] **Step 1: Update the deployment invariant**

Document the exact first-request/challenge/retry/clearance-reuse flow. Replace “always validates the token” with “validates every token that is generated.” Include the exact Managed Challenge expression, `managed` pre-clearance, `TURNSTILE_HOSTNAMES=oginstagram.com`, and the fact that Challenge Passage is a maximum lifetime while precursor clearance may invalidate the cookie earlier.

- [ ] **Step 2: Check the diff for accidental scope**

Run:

```bash
git diff --check
git diff -- worker/turnstile.ts worker/preview.ts worker/turnstile.test.mjs web/src/preview-request.ts web/src/preview.tsx wrangler.jsonc worker/wrangler.test.jsonc .dev.vars.example tsconfig.json worker-configuration.d.ts README.md
```

Expected: no whitespace errors; only the approved pre-clearance flow changes inside the already-dirty files.

- [ ] **Step 3: Run the repository verification**

Run:

```bash
pnpm run check
```

Expected: Wrangler types, lint, both TypeScript checks, Node tests, and Go tests all exit 0.

- [ ] **Step 4: Review against Cloudflare Workers best practices**

Retrieve the current official Workers best-practices page and compare the final Worker diff for request-body limits, secrets, outbound fetch handling, global state, and observability. Fix only findings caused by this change, then rerun `pnpm run check`.
