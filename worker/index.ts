import { Container } from "@cloudflare/containers";
import { Hono } from "hono";
import { botRE, homePathLocale, parseEmbedSegments, resolveHomeLocale, splitPath } from "../shared/routes";

const instagramOrigin = "https://www.instagram.com";
const defaultCache = (caches as unknown as { default: Cache }).default;

export class OgUsContainer extends Container<Env> {
  defaultPort = 8080;
  sleepAfter = "10m";
  enableInternet = true;

  async fetch(request: Request): Promise<Response> {
    this.envVars = containerEnv(this.env);
    return super.fetch(request);
  }
}

const CONTAINER_NAME = "oginstagram-us";
const CONTAINER_HINT: DurableObjectLocationHint = "enam";

const STATUS_PATH = "/_status";

const app = new Hono<{ Bindings: Env }>();

app.all("*", async c => {
  const request = c.req.raw;
  const env = c.env;
  const ctx = c.executionCtx as ExecutionContext;
  const started = Date.now();
  const url = new URL(request.url);
  if (url.pathname === "/_worker/health") {
    return healthResponse(env);
  }
  if (url.pathname === STATUS_PATH) {
    return serveStatus(env, ctx, url);
  }

  const meta: RequestMeta = { cacheHit: false };
  let response: Response;
  try {
    response = await handleAppRequest(request, env, ctx, url, meta);
  } catch (err) {
    logRequestMetric(request, url, Date.now() - started, 500, false, env.AE, meta.cacheHit, "exception");
    throw err;
  }
  const metricStatus = meta.metricStatus ?? response.status;
  logRequestMetric(request, url, Date.now() - started, metricStatus, metricStatus < 400, env.AE, meta.cacheHit, meta.reason);
  return response;
});

export default app;

type RequestMeta = { cacheHit: boolean; metricStatus?: number; reason?: string };

async function handleAppRequest(request: Request, env: Env, ctx: ExecutionContext, url: URL, meta: RequestMeta): Promise<Response> {
  const route = resolveContainerRoute(url);
  if (!route) {
    return new Response(null, { status: 404 });
  }

  if (route.humanRedirect && !botRE.test(request.headers.get("user-agent") ?? "")) {
    return Response.redirect(route.humanRedirect, 307);
  }

  const cacheKey = edgeCacheKey(route, request, url);
  if (cacheKey) {
    const hit = await defaultCache.match(cacheKey);
    if (hit) {
      meta.cacheHit = true;
      return hit;
    }
  }

  const containerRequest = route.rewritePath
    ? new Request(new URL(route.rewritePath, url.origin).href, request)
    : request;
  let response = await containerInstance(env).fetch(containerRequest);

  if (response.headers.get("x-og-cache") === "hit") {
    meta.cacheHit = true;
  }

  const ogStatus = response.headers.get("x-og-status");
  if (ogStatus) {
    meta.metricStatus = Number.parseInt(ogStatus, 10) || undefined;
  }

  const ogReason = response.headers.get("x-og-reason");
  if (ogReason) {
    meta.reason = ogReason;
  }

  if (
    response.headers.has("x-og-config") ||
    response.headers.has("x-og-cache") ||
    response.headers.has("x-og-status") ||
    response.headers.has("x-og-reason")
  ) {
    response = new Response(response.body, response);
    response.headers.delete("x-og-config");
    response.headers.delete("x-og-cache");
    response.headers.delete("x-og-status");
    response.headers.delete("x-og-reason");
  }

  if (cacheKey && response.headers.get("Cache-Control")?.includes("s-maxage")) {
    ctx.waitUntil(defaultCache.put(cacheKey, response.clone()));
  }
  return response;
}

function containerInstance(env: Env): DurableObjectStub<Container<Env>> {
  const ns = env.OG_CONTAINER as DurableObjectNamespace<Container<Env>>;
  return ns.get(ns.idFromName(CONTAINER_NAME), { locationHint: CONTAINER_HINT });
}

function containerEnv(env: Env): Record<string, string> {
  return {
    DECODO_USERNAME: env.DECODO_USERNAME ?? "",
    DECODO_PASSWORD: env.DECODO_PASSWORD ?? "",
    BASE_URL: env.BASE_URL ?? "",
    BRAND_NAME: env.BRAND_NAME ?? "",
    BRAND_COLOR: env.BRAND_COLOR ?? "",
    SUPPORT_URL: env.SUPPORT_URL ?? "",
    GITHUB_URL: env.GITHUB_URL ?? "",
    OG_VERSION: versionCode(env)
  };
}

function logRequestMetric(
  request: Request,
  url: URL,
  ms: number,
  status: number,
  ok: boolean,
  ae?: AnalyticsEngineDataset,
  cacheHit = false,
  reason?: string
): void {
  const route = routeMetricName(url);
  if (!isMetricsRoute(route)) {
    return;
  }
  const client = clientClass(request.headers.get("user-agent") ?? "");
  if (client === "human") {
    return;
  }
  const outcome = ok ? "ok" : "fail";
  const cache = cacheHit ? "hit" : "miss";
  const reasonBlob = reason || (ok ? "ok" : "fail");
  console.log({ event: "http_request", route, client, outcome, status, ms, cache, reason: reasonBlob, path: url.pathname });
  if (cacheHit) {
    return;
  }
  ae?.writeDataPoint({
    blobs: [route, client, outcome, reasonBlob],
    doubles: [ms, ok ? 1 : 0, status],
    indexes: [route]
  });
}

function isMetricsRoute(route: string): boolean {
  return route === "embed" || route === "direct";
}

function hasProxyConfig(env: Env): boolean {
  return Boolean(env.DECODO_USERNAME?.trim() && env.DECODO_PASSWORD?.trim());
}

function healthResponse(env: Env): Response {
  const proxyConfig = hasProxyConfig(env);
  return Response.json(
    {
      ok: proxyConfig,
      service: "oginstagram-worker",
      version: versionCode(env),
      proxy_config: proxyConfig
    },
    { status: proxyConfig ? 200 : 503 }
  );
}

function versionCode(env: Env): string {
  return env.CF_VERSION_METADATA?.id?.replaceAll("-", "").slice(0, 8) || "dev";
}

type ContainerRoute = {
  cacheKey: string | null;
  varyBot: boolean;
  varyLang?: boolean;
  humanRedirect: string | null;
  rewritePath?: string;
};

function isDirectHost(url: URL): boolean {
  return url.hostname.startsWith("d.");
}

function isGalleryHost(url: URL): boolean {
  return url.hostname.startsWith("g.");
}

function resolveContainerRoute(url: URL): ContainerRoute | null {
  const path = url.pathname;
  if (path === "/") {
    return { cacheKey: "/__home1", varyBot: false, varyLang: true, humanRedirect: null };
  }
  if (path === "/_container/health") {
    return { cacheKey: null, varyBot: false, humanRedirect: null };
  }

  const pathLocale = homePathLocale(path);
  if (pathLocale) {
    return { cacheKey: `/__home1/${pathLocale}`, varyBot: false, humanRedirect: null };
  }
  if (path === "/favicon.ico" || /^\/favicon-\d+\.png$/.test(path)) {
    return { cacheKey: path, varyBot: false, humanRedirect: null };
  }
  if (path === "/uplot.js" || path === "/uplot.css") {
    return { cacheKey: path, varyBot: false, humanRedirect: null };
  }
  if (path === "/main.js" || path === "/main.css") {
    return { cacheKey: path, varyBot: false, humanRedirect: null };
  }
  if (path === "/oembed" || path === "/owoembed") {
    return { cacheKey: `${path}?${sortedSearch(url)}`, varyBot: false, humanRedirect: null };
  }

  const segments = splitPath(path);
  if ((segments.length === 2 || segments.length === 3) && segments[0] === "offload") {
    const thumbnail = url.searchParams.has("thumbnail") ? "?thumbnail=1" : "";
    return { cacheKey: `${path}${thumbnail}`, varyBot: false, humanRedirect: null };
  }
  if (
    (segments.length === 2 && segments[0] === "statuses") ||
    (segments.length === 4 && segments[0] === "api" && segments[1] === "v1" && segments[2] === "statuses") ||
    (segments.length === 4 && segments[0] === "users" && segments[2] === "statuses")
  ) {
    return { cacheKey: `${path}?${sortedSearch(url)}`, varyBot: false, humanRedirect: null };
  }
  if (segments.length === 2 && segments[0] === "users") {
    return { cacheKey: path, varyBot: false, humanRedirect: null };
  }

  const embed = parseEmbedSegments(segments);
  if (embed) {
    const selected = mediaSelection(url.searchParams, embed.pathIndex);
    if (isDirectHost(url)) {
      return {
        cacheKey: `/direct/${encodeURIComponent(embed.shortcode)}/${selected ?? "-"}`,
        varyBot: false,
        humanRedirect: null,
        rewritePath: `/offload/${encodeURIComponent(embed.shortcode)}/${(selected ?? 0) + 1}`
      };
    }
    const gallery = isGalleryHost(url);
    let origin = `${instagramOrigin}/${embed.postType}/${encodeURIComponent(embed.shortcode)}/`;
    if (selected !== null) {
      origin += `?img_index=${selected + 1}`;
    }
    return {
      cacheKey: `/${gallery ? "gallery" : "embed"}/${embed.postType}/${encodeURIComponent(embed.shortcode)}/${selected ?? "-"}`,
      varyBot: true,
      humanRedirect: origin,
      rewritePath: gallery ? galleryRewritePath(url) : undefined
    };
  }
  return null;
}

function galleryRewritePath(url: URL): string {
  const rewritten = new URL(url.pathname + url.search, url.origin);
  rewritten.searchParams.set("__gallery", "1");
  return rewritten.pathname + rewritten.search;
}

function edgeCacheKey(route: ContainerRoute, request: Request, url: URL): string | null {
  if (!route.cacheKey) {
    return null;
  }
  let key = `${url.origin}/__edge${route.cacheKey}`;
  if (route.varyBot) {
    key += `${key.includes("?") ? "&" : "?"}bot=${botClass(request.headers.get("user-agent") ?? "")}`;
  }
  if (route.varyLang) {
    const locale = resolveHomeLocale(request.headers.get("accept-language") ?? "");
    key += `${key.includes("?") ? "&" : "?"}lang=${locale}`;
  }
  return key;
}

function botClass(userAgent: string): string {
  if (/discordbot/i.test(userAgent)) {
    return "discord";
  }
  if (/telegrambot/i.test(userAgent)) {
    return "telegram";
  }
  return "bot";
}

function clientClass(userAgent: string): string {
  if (!botRE.test(userAgent)) {
    return "human";
  }
  return botClass(userAgent);
}

function routeMetricName(url: URL): string {
  const path = url.pathname;
  if (path === "/" || homePathLocale(path)) {
    return "home";
  }
  if (path === "/_container/health") {
    return "container-health";
  }
  if (path === "/favicon.ico" || /^\/favicon-\d+\.png$/.test(path)) {
    return "icon";
  }
  if (path === "/oembed" || path === "/owoembed") {
    return "oembed";
  }
  const segments = splitPath(path);
  if ((segments.length === 2 || segments.length === 3) && segments[0] === "offload") {
    return "offload";
  }
  if (
    (segments.length === 2 && segments[0] === "statuses") ||
    (segments.length === 4 && segments[0] === "api" && segments[1] === "v1" && segments[2] === "statuses") ||
    (segments.length === 4 && segments[0] === "users" && segments[2] === "statuses")
  ) {
    return "activity";
  }
  if (segments.length === 2 && segments[0] === "users") {
    return "activity-user";
  }
  if (!parseEmbedSegments(segments)) {
    return "not-found";
  }
  return isDirectHost(url) ? "direct" : "embed";
}

function mediaSelection(params: URLSearchParams, pathIndex: number | null): number | null {
  if (pathIndex !== null) {
    return Math.max(0, pathIndex - 1);
  }
  const imgIndex = queryInt(params, "img_index");
  if (imgIndex !== null) {
    return Math.max(0, imgIndex - 1);
  }
  const index = queryInt(params, "index");
  if (index !== null) {
    return Math.max(0, index);
  }
  const order = queryInt(params, "order");
  if (order !== null) {
    return Math.max(0, order);
  }
  return null;
}

function queryInt(params: URLSearchParams, key: string): number | null {
  if (!params.has(key)) {
    return null;
  }
  const parsed = Number.parseInt(params.get(key) ?? "", 10);
  return Number.isFinite(parsed) && parsed >= 0 ? parsed : 0;
}

function sortedSearch(url: URL): string {
  const entries = [...url.searchParams.entries()].sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0));
  return new URLSearchParams(entries).toString();
}

const STATUS_QUERY =
  "SELECT intDiv(toUInt32(timestamp), 600) * 600 AS t, " +
  "sum(_sample_interval * double1) / sum(_sample_interval) AS latency, " +
  "sumIf(_sample_interval, blob3 = 'ok') AS resolved, " +
  "sumIf(_sample_interval, blob3 = 'fail') AS failed " +
  "FROM oginstagram_requests WHERE timestamp > NOW() - INTERVAL '1' DAY GROUP BY t ORDER BY t";

type StatusSeries = {
  t: number[];
  latency: number[];
  resolved: number[];
  failed: number[];
};

async function serveStatus(env: Env, ctx: ExecutionContext, url: URL): Promise<Response> {
  const cacheKey = `${url.origin}/__status`;
  const hit = await defaultCache.match(cacheKey);
  if (hit) {
    return hit;
  }
  if (!env.AE_ACCOUNT_ID || !env.AE_API_TOKEN) {
    return statusJSON(emptyStatusSeries(), 503);
  }
  let rows: Array<{ t: unknown; latency: unknown; resolved: unknown; failed: unknown }> = [];
  try {
    const upstream = await fetch(
      `https://api.cloudflare.com/client/v4/accounts/${env.AE_ACCOUNT_ID}/analytics_engine/sql`,
      {
        method: "POST",
        headers: { Authorization: `Bearer ${env.AE_API_TOKEN}` },
        body: `${STATUS_QUERY} FORMAT JSON`
      }
    );
    if (!upstream.ok) {
      console.error({ event: "status_query_failed", status: upstream.status, body: await upstream.text() });
      return statusJSON(emptyStatusSeries(), 502);
    }
    const parsed = (await upstream.json()) as {
      data?: Array<{ t: unknown; latency: unknown; resolved: unknown; failed: unknown }>
    };
    rows = parsed.data ?? [];
  } catch (err) {
    console.error({ event: "status_query_error", error: err instanceof Error ? err.message : String(err) });
    return statusJSON(emptyStatusSeries(), 502);
  }
  const series = {
    t: rows.map(row => Number(row.t)),
    latency: rows.map(row => Math.round(Number(row.latency) * 100) / 100),
    resolved: rows.map(row => Math.round(Number(row.resolved) * 100) / 100),
    failed: rows.map(row => Math.round(Number(row.failed) * 100) / 100)
  };
  const response = statusJSON(series, 200, "public, s-maxage=60");
  ctx.waitUntil(defaultCache.put(cacheKey, response.clone()));
  return response;
}

function emptyStatusSeries(): StatusSeries {
  return { t: [], latency: [], resolved: [], failed: [] };
}

function statusJSON(series: StatusSeries, status: number, cacheControl?: string): Response {
  const headers: Record<string, string> = { "content-type": "application/json" };
  if (cacheControl) {
    headers["Cache-Control"] = cacheControl;
  }
  return new Response(JSON.stringify(series), { status, headers });
}
