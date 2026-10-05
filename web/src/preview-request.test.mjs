import assert from "node:assert/strict";
import test from "node:test";
import { requestPreview } from "./preview-request.ts";

test("first request is tokenless and surfaces the Cloudflare challenge; retry carries the token", async () => {
  await assert.rejects(requestPreview("/p/Ab_12", undefined, new AbortController().signal, async (target, init) => {
    assert.equal(target, "/api/embed");
    assert.deepEqual(JSON.parse(init.body), { path: "/p/Ab_12" });
    return new Response("challenge", { headers: { "cf-mitigated": "challenge" } });
  }), { message: "cloudflare-challenge" });

  const response = await requestPreview("/p/Ab_12", "token", new AbortController().signal, async (_target, init) => {
    assert.deepEqual(JSON.parse(init.body), { path: "/p/Ab_12", token: "token" });
    return new Response("preview", { status: 403 });
  });
  assert.equal(response.status, 403);
});
