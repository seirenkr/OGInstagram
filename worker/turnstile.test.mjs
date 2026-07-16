import assert from "node:assert/strict";
import { after, before, test } from "node:test";
import { unstable_startWorker } from "wrangler";

let worker;

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

  assert.equal(response.status, 403);
  assert.deepEqual(await response.json(), { error: "clearance required" });
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

  assert.equal(response.status, 403);
  assert.deepEqual(await response.json(), { error: "verification failed" });
});
