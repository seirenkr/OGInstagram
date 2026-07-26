import assert from "node:assert/strict";
import test from "node:test";
import {
  byteLeaseGrant,
  containerApiBaseURL,
  handleProxyBudget,
  proxyByteLeaseSize,
  proxyBudgetExpiresHeader,
  proxyBudgetGrantedBytesHeader,
  proxyDailyByteBudget,
  proxyMonthlyBudgetBytes,
  resolveContainerApi,
} from "./budget.ts";

test("monthly traffic is divided equally across each UTC day", () => {
  assert.equal(proxyDailyByteBudget(new Date("2026-02-10T12:00:00Z")), 3_571_428_571);
  assert.equal(proxyDailyByteBudget(new Date("2024-02-10T12:00:00Z")), 3_448_275_862);
  assert.equal(proxyDailyByteBudget(new Date("2026-04-10T12:00:00Z")), 3_333_333_333);
  assert.equal(proxyDailyByteBudget(new Date("2026-07-10T12:00:00Z")), 3_225_806_451);
  assert.equal(proxyMonthlyBudgetBytes, 100_000_000_000);
  assert.equal(proxyBudgetExpiresHeader, "Oginstagram-Proxy-Budget-Expires");
  assert.equal(proxyBudgetGrantedBytesHeader, "Oginstagram-Proxy-Budget-Granted-Bytes");
});

test("byte leases are capped at the trusted lease size", () => {
  const dailyBudget = proxyDailyByteBudget(new Date("2026-07-10T12:00:00Z"));
  assert.equal(
    byteLeaseGrant(0, dailyBudget, proxyByteLeaseSize),
    proxyByteLeaseSize
  );
  assert.equal(
    byteLeaseGrant(dailyBudget - 3, dailyBudget, proxyByteLeaseSize),
    3
  );
});

test("an exhausted or invalid budget never grants a lease", () => {
  const dailyBudget = proxyDailyByteBudget(new Date("2026-07-10T12:00:00Z"));
  assert.equal(
    byteLeaseGrant(dailyBudget, dailyBudget, proxyByteLeaseSize),
    0
  );
  assert.equal(
    byteLeaseGrant(dailyBudget + 1, dailyBudget, proxyByteLeaseSize),
    0
  );
  for (const [used, limit, requested] of [
    [-1, dailyBudget, proxyByteLeaseSize],
    [0, 0, proxyByteLeaseSize],
    [0, dailyBudget, 0],
    [0.5, dailyBudget, proxyByteLeaseSize],
  ]) {
    assert.equal(byteLeaseGrant(used, limit, requested), 0);
  }
});

function budgetEnv(result) {
  return {
    PROXY_BUDGET: {
      getByName: () => ({
        take: async () => {
          if (result instanceof Error) throw result;
          return result;
        },
      }),
    },
  };
}

test("budget handler returns a bounded lease protocol", async () => {
  const expiresAt = Date.now() + 60_000;
  const response = await handleProxyBudget(
    new Request(`${containerApiBaseURL}/budget`, { method: "POST" }),
    budgetEnv({ bucket: 1, expiresAt, grantedBytes: 7 })
  );
  assert.equal(response.status, 204);
  assert.equal(response.headers.get(proxyBudgetGrantedBytesHeader), "7");
  assert.equal(response.headers.get(proxyBudgetExpiresHeader), String(expiresAt));
});

test("budget handler distinguishes exhaustion and backend failure", async () => {
  const expiresAt = Date.now() + 60_000;
  const exhausted = await handleProxyBudget(
    new Request(`${containerApiBaseURL}/budget`, { method: "POST" }),
    budgetEnv({ bucket: 1, expiresAt, grantedBytes: 0 })
  );
  assert.equal(exhausted.status, 429);
  assert.equal(exhausted.headers.get(proxyBudgetExpiresHeader), String(expiresAt));

  const originalError = console.error;
  console.error = () => {};
  try {
    const failed = await handleProxyBudget(
      new Request(`${containerApiBaseURL}/budget`, { method: "POST" }),
      budgetEnv(new Error("storage unavailable"))
    );
    assert.equal(failed.status, 503);
  } finally {
    console.error = originalError;
  }
});

test("container internal endpoint dispatches cache and budget by path", () => {
  assert.equal(resolveContainerApi(new Request(`${containerApiBaseURL}/cache`)), "cache");
  assert.equal(resolveContainerApi(new Request(`${containerApiBaseURL}/budget`)), "budget");
  assert.equal(resolveContainerApi(new Request(`${containerApiBaseURL}/unknown`)), null);
});
