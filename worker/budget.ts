// Keep this policy in the trusted Worker runtime. DataImpulse prices traffic
// in decimal GB, so 100 GB is 100,000,000,000 bytes. Each UTC day receives an
// equal share of that month's allowance; unused bytes do not carry forward.
export const proxyMonthlyBudgetBytes = 100_000_000_000;
export const proxyByteLeaseSize = 1 << 20;
export const proxyBudgetExpiresHeader = "Oginstagram-Proxy-Budget-Expires";
export const proxyBudgetGrantedBytesHeader = "Oginstagram-Proxy-Budget-Granted-Bytes";
export const containerApiBaseURL = "http://og.do";

export type ProxyByteLease = {
  bucket: number;
  expiresAt: number;
  grantedBytes: number;
};

export function resolveContainerApi(request: Request): "cache" | "budget" | null {
  switch (new URL(request.url).pathname) {
    case "/cache": return "cache";
    case "/budget": return "budget";
    default: return null;
  }
}

export function proxyDailyByteBudget(now: Date): number {
  const year = now.getUTCFullYear();
  const month = now.getUTCMonth();
  const daysInMonth = new Date(Date.UTC(year, month + 1, 0)).getUTCDate();
  return Math.floor(proxyMonthlyBudgetBytes / daysInMonth);
}

export function byteLeaseGrant(used: number, limit: number, requested: number): number {
  if (
    !Number.isSafeInteger(used) || used < 0
    || !Number.isSafeInteger(limit) || limit <= 0
    || !Number.isSafeInteger(requested) || requested <= 0
  ) {
    return 0;
  }
  return Math.min(requested, Math.max(0, limit - used));
}

export async function handleProxyBudget(
  request: Request,
  env: Env
): Promise<Response> {
  if (request.method !== "POST") {
    return new Response(null, { status: 405, headers: { allow: "POST" } });
  }

  const stub = env.PROXY_BUDGET.getByName("global", { locationHint: "enam" });
  try {
    const lease = await stub.take(proxyByteLeaseSize);
    if (lease.grantedBytes === 0) {
      return new Response(null, {
        status: 429,
        headers: { [proxyBudgetExpiresHeader]: String(lease.expiresAt) },
      });
    }
    return new Response(null, {
      status: 204,
      headers: {
        [proxyBudgetExpiresHeader]: String(lease.expiresAt),
        [proxyBudgetGrantedBytesHeader]: String(lease.grantedBytes),
      },
    });
  } catch (error) {
    console.error({
      event: "proxy_budget_failed",
      "error.type": "budget_backend_error",
      "exception.message": error instanceof Error ? error.message : String(error),
    });
    return new Response(null, { status: 503 });
  }
}
