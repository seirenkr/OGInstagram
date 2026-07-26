import { Container, getContainer } from "@cloudflare/containers";
import { DurableObject } from "cloudflare:workers";
import {
  byteLeaseGrant,
  containerApiBaseURL,
  handleProxyBudget,
  proxyDailyByteBudget,
  resolveContainerApi,
  type ProxyByteLease,
} from "./budget";
import { handleCache } from "./cache";
import { internalHeader, versionCode } from "./http";

const maxContainerInFlight = 48;

export class OgUsContainer extends Container<Env> {
  defaultPort = 8080;
  private inFlight = 0;

  async fetch(request: Request): Promise<Response> {
    // The Go server has a smaller origin-fetch semaphore. This outer bound
    // prevents an unbounded queue of random cache misses from occupying the
    // single container after callers have already timed out.
    if (this.inFlight >= maxContainerInFlight) {
      return new Response(null, {
        status: 503,
        headers: {
          "cache-control": "no-store",
          "retry-after": "1",
          [internalHeader.errorType]: "overloaded",
        },
      });
    }
    this.inFlight++;
    try {
      this.envVars = containerEnv(this.env);
      return await super.fetch(request);
    } finally {
      this.inFlight--;
    }
  }
}

// Strict daily byte cap for outbound proxy traffic, kept on its own small DO
// so budget RPCs never queue behind container proxy or cache traffic. Durable
// Object input gates make the get-then-put below atomic, and the output gate
// holds each response until the write is durable. A restart can waste unused
// leased bytes, but cannot reset or exceed the cap.
export class ProxyBudget extends DurableObject<Env> {
  async take(requested: number): Promise<ProxyByteLease> {
    const now = new Date();
    const bucket = Math.floor(now.getTime() / 86_400_000);
    const limit = proxyDailyByteBudget(now);
    const state = await this.ctx.storage.get<{ bucket: number; used: number }>("state");
    const used = state?.bucket === bucket ? state.used : 0;
    const granted = byteLeaseGrant(used, limit, requested);
    if (granted > 0) {
      await this.ctx.storage.put("state", { bucket, used: used + granted });
    }
    return { bucket, expiresAt: (bucket + 1) * 86_400_000, grantedBytes: granted };
  }
}

// Keep both internal APIs on one intercepted host; production container
// routing does not reliably install more than the first host mapping.
OgUsContainer.outboundByHost = {
  [new URL(containerApiBaseURL).host]: handleContainerApi,
};

function handleContainerApi(request: Request, env: Env): Promise<Response> {
  switch (resolveContainerApi(request)) {
    case "cache": return handleCache(request, env);
    case "budget": return handleProxyBudget(request, env);
    default: return Promise.resolve(new Response(null, { status: 404 }));
  }
}

export function containerStub(env: Env): DurableObjectStub<OgUsContainer> {
  return getContainer(env.OG_CONTAINER);
}

function containerEnv(env: Env): Record<string, string> {
  return {
    PROXY_USERNAME: env.PROXY_USERNAME ?? "",
    PROXY_PASSWORD: env.PROXY_PASSWORD ?? "",
    BASE_URL: env.BASE_URL ?? "",
    OFFLOAD_SIGNING_KEYS: env.OFFLOAD_SIGNING_KEYS ?? "",
    CACHE_URL: `${containerApiBaseURL}/cache`,
    BUDGET_URL: `${containerApiBaseURL}/budget`,
    OG_VERSION: versionCode(env),
  };
}
