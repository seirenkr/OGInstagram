import { WorkerEntrypoint } from "cloudflare:workers";
import { resolveContainerRoute } from "../shared/routes";
import { containerStub } from "./container";
import { ensureCacheControl, internalHeader, overallBudgetMs } from "./http";
import { serveStatus } from "./observability";

// Cached entrypoint. The default gateway is intentionally uncached and calls
// this export with an explicit cf.cacheKey for every cacheable route.
export class Embed extends WorkerEntrypoint<Env> {
  async purgeAll(): Promise<CachePurgeResult> {
    if (!this.ctx.cache) {
      return { success: false, errors: [{ code: 0, message: "cache context unavailable" }] };
    }
    return this.ctx.cache.purge({ purgeEverything: true });
  }

  async fetch(request: Request): Promise<Response> {
    return ensureCacheControl(await this.handle(request));
  }

  private async handle(request: Request): Promise<Response> {
    const url = new URL(request.url);
    const requestId = request.headers.get(internalHeader.requestId) ?? crypto.randomUUID();

    if (url.pathname === "/api/status") return serveStatus(this.env, requestId);

    const route = resolveContainerRoute(url);
    if (!route) {
      return new Response(null, { status: 404, headers: { "cache-control": "no-store" } });
    }

    const routedRequest = route.rewritePath
      ? new Request(new URL(route.rewritePath, url.origin), request)
      : request;
    const headers = new Headers(routedRequest.headers);
    const signedOffload = route.offloadPath !== undefined
      && headers.get(internalHeader.signedOffload) === "1";
    // Gateway stamps these values from the eyeball request. Never derive links
    // from client-supplied internal headers.
    if (!headers.has(internalHeader.publicOrigin)) {
      headers.set(internalHeader.publicOrigin, url.origin);
    }
    headers.set(internalHeader.requestId, requestId);
    // Only a route resolved by the edge may authorize a first-time fetch.
    headers.delete(internalHeader.allowOriginFetch);
    headers.delete(internalHeader.signedOffload);
    if (route.allowOffloadFetch || signedOffload) headers.set(internalHeader.allowOriginFetch, "1");

    // The container applies its own flat timeout; this only catches a hung
    // container, so it gets a small extra allowance.
    let response: Response;
    try {
      response = await containerStub(this.env).fetch(
        new Request(routedRequest, { headers }),
        { signal: AbortSignal.timeout(overallBudgetMs + 500) }
      );
    } catch (error) {
      if (error instanceof DOMException && error.name === "TimeoutError") {
        return new Response(null, {
          status: 504,
          headers: {
            "cache-control": "no-store",
            [internalHeader.originStatus]: "504",
            [internalHeader.errorType]: "container_timeout",
          },
        });
      }
      throw error;
    }

    const originStatus = Number(response.headers.get(internalHeader.originStatus));
    if (originStatus === 499 || originStatus >= 500) {
      // Error-card HTML uses HTTP 200 for crawler compatibility. The internal
      // status is authoritative for cache safety.
      const safe = new Response(response.body, response);
      safe.headers.set("cache-control", "no-store");
      safe.headers.delete("cloudflare-cdn-cache-control");
      return safe;
    }
    return response;
  }
}
