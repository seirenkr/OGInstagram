import { botPattern, edgeCacheKey, resolveContainerRoute } from "../shared/routes";
import { serveHome, serviceBaseUrl } from "./home";
import { internalHeader, problemResponse, stampGatewayHeaders, stripInternalHeaders } from "./http";
import { proxyOffloadMedia } from "./media";
import { logRequestMetric, type RequestMeta } from "./observability";
import { authorizeOffload } from "./offload";
import { isValidAdminToken, serveEmbed } from "./preview";

export { ContainerProxy } from "@cloudflare/containers";
export { OgUsContainer, ProxyBudget } from "./container";
export { Embed } from "./embed";

export default {
  async fetch(request, env, ctx): Promise<Response> {
    const started = Date.now();
    const url = new URL(request.url);
    const ray = request.headers.get("cf-ray");
    const requestId = ray && ray.length <= 128 ? ray : crypto.randomUUID();

    if (url.pathname === "/api/status") {
      if (request.method !== "GET" && request.method !== "HEAD") return problemResponse(405, "method not allowed", { allow: "GET, HEAD" });
      const cacheOrigin = serviceBaseUrl(env.BASE_URL, url).origin;
      return ctx.exports.Embed.fetch(stampGatewayHeaders(request, url, requestId), {
        cf: { cacheKey: `${cacheOrigin}/__status` },
      });
    }

    if (url.pathname === "/api/admin/purge") {
      if (request.method !== "POST") return problemResponse(405, "method not allowed", { allow: "POST" });
      if (!(await isValidAdminToken(request, env))) {
        return problemResponse(403, "A valid administrator bearer token is required.");
      }
      const result = await ctx.exports.Embed.purgeAll();
      if (!result.success) {
        return problemResponse(502, "The edge cache could not be purged.", {}, { errors: result.errors });
      }
      return Response.json(result, {
        headers: { "cache-control": "no-store" },
      });
    }

    if (url.pathname === "/api/embed") {
      return withServerTiming(
        await serveEmbed(request, env, ctx, url, requestId),
        Date.now() - started
      );
    }

    if (url.pathname === "/") {
      if (request.method !== "GET" && request.method !== "HEAD") return problemResponse(405, "method not allowed", { allow: "GET, HEAD" });
      return serveHome(request, env, url);
    }

    const route = resolveContainerRoute(url);
    if (!route) return new Response(null, { status: 404, headers: { "cache-control": "no-store" } });
    if (request.method !== "GET" && request.method !== "HEAD") return problemResponse(405, "method not allowed", { allow: "GET, HEAD" });
    if (route.humanRedirect && !botPattern.test(request.headers.get("user-agent") ?? "")) {
      return Response.redirect(route.humanRedirect, 307);
    }

    if (route.offloadPath) {
      let authorized;
      try {
        authorized = await authorizeOffload(url, route.offloadPath, env.OFFLOAD_SIGNING_KEYS);
      } catch (error) {
        console.error({
          event: "offload_signing_config_error",
          request_id: requestId,
          "error.type": "invalid_configuration",
          "exception.message": error instanceof Error ? error.message : String(error),
        });
        return withServerTiming(new Response(null, {
          status: 503,
          headers: { "cache-control": "no-store", "retry-after": "1" },
        }), Date.now() - started);
      }
      if (!authorized) {
        return withServerTiming(new Response(null, {
          status: 404,
          headers: { "cache-control": "no-store" },
        }), Date.now() - started);
      }
    }

    const meta: RequestMeta = { cacheHit: false, requestId, metric: route.metric };
    const cacheKey = edgeCacheKey(route, url);

    let response: Response;
    try {
      response = await ctx.exports.Embed.fetch(stampGatewayHeaders(request, url, requestId, {
        signedOffload: route.offloadPath !== undefined,
      }), {
        cf: { cacheKey },
      });
    } catch (error) {
      meta.errorType = "exception";
      logRequestMetric(request, url, meta, Date.now() - started, 500, false, env.AE);
      throw error;
    }

    const edgeHit = isEdgeServed(response.headers.get("cf-cache-status"));
    const modelHit = response.headers.get(internalHeader.cacheStatus) === "hit";
    meta.cacheHit = edgeHit || modelHit;
    meta.cacheTier = edgeHit ? "edge" : modelHit ? "model" : "origin";

    const internalStatus = response.headers.get(internalHeader.originStatus);
    if (internalStatus) meta.metricStatus = Number.parseInt(internalStatus, 10) || undefined;
    const originMs = Number.parseInt(response.headers.get(internalHeader.originDuration) ?? "", 10);
    if (Number.isFinite(originMs)) meta.originMs = originMs;
    const errorType = response.headers.get(internalHeader.errorType);
    if (errorType) meta.errorType = errorType;
    response = stripInternalHeaders(response);

    // While its 14-day capability is valid, /offload remains a stable
    // indirection that can resolve a fresh Instagram CDN URL.
    if (response.status === 302
      && url.pathname.startsWith("/offload/")
      && request.headers.get("sec-fetch-site") === "same-origin") {
      response = await proxyOffloadMedia(
        response,
        url.searchParams.get("preview") === "avatar"
          ? "avatar"
          : url.searchParams.get("preview") === "1" ? "media" : null,
        request.headers.get("accept") ?? "",
        requestId
      );
    }

    const ms = Date.now() - started;
    const metricStatus = meta.metricStatus ?? response.status;
    logRequestMetric(request, url, meta, ms, metricStatus, metricStatus < 400, env.AE);
    return withServerTiming(response, ms);
  },
} satisfies ExportedHandler<Env>;

function withServerTiming(response: Response, ms: number): Response {
  const timed = new Response(response.body, response);
  timed.headers.append("server-timing", `gateway;dur=${ms}`);
  return timed;
}

function isEdgeServed(cacheStatus: string | null): boolean {
  return cacheStatus === "HIT" || cacheStatus === "UPDATING"
    || cacheStatus === "STALE" || cacheStatus === "REVALIDATED";
}
