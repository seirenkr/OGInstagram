import { Container, type OutboundHandlerContext } from "@cloudflare/containers";
export { ContainerProxy } from "@cloudflare/containers";
import { asHomeLocale, botRE, mediaSelection, parseEmbedSegments, parseStoriesSegments, resolveHomeLocale, splitPath, validEmbedPath, validUsername } from "../shared/routes";

const instagramOrigin = "https://www.instagram.com";
const defaultCache = (caches as CacheStorage & { default: Cache }).default;
const maxEmbedBodyBytes = 4096;
const maxModelCacheBodyBytes = 1900000;
const knownMediaTtlMs = 30 * 24 * 60 * 60 * 1000;
const modelCacheKeyRE = /^(?:post:[A-Za-z0-9_-]{1,24}|profile:[A-Za-z0-9._]{1,30}|story:[A-Za-z0-9._]{1,30}\/[0-9]{1,32})$/;

type ModelCacheRow = { value: string; expires_at: number };
type StatusCacheRow = { value: string; expires_at: number; retry_at: number };

type StatusResult = {
  series: StatusSeries;
  status: number;
  cacheSeconds: number;
  stale: boolean;
};

export class OgUsContainer extends Container<Env> {
  defaultPort = 8080;
  private statusRefresh?: Promise<StatusResult>;

  constructor(ctx: DurableObjectState<{}>, env: Env) {
    super(ctx, env);
    ctx.blockConcurrencyWhile(async () => {
      this.ctx.storage.transactionSync(() => {
        const sql = this.ctx.storage.sql;
        // ponytail: no migrations — every table here is a disposable cache. A DB
        // from the old migration-ledger deploys is dropped wholesale and rebuilt.
        if (sql.exec("SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = '_sql_schema_migrations'").toArray().length) {
          sql.exec("DROP TABLE _sql_schema_migrations; DROP TABLE IF EXISTS model_cache; DROP TABLE IF EXISTS known_media; DROP TABLE IF EXISTS proxy_budget; DROP TABLE IF EXISTS status_cache;");
        }
        sql.exec(`
        CREATE TABLE IF NOT EXISTS model_cache (
          key TEXT PRIMARY KEY,
          value TEXT NOT NULL,
          expires_at INTEGER NOT NULL
        );
        CREATE INDEX IF NOT EXISTS model_cache_expires ON model_cache(expires_at);
        CREATE TABLE IF NOT EXISTS known_media (
          key TEXT PRIMARY KEY,
          seen_at INTEGER NOT NULL
        ) WITHOUT ROWID;
        CREATE TABLE IF NOT EXISTS proxy_budget (
          bucket INTEGER PRIMARY KEY,
          used INTEGER NOT NULL
        ) WITHOUT ROWID;
        CREATE TABLE IF NOT EXISTS status_cache (
          id INTEGER PRIMARY KEY CHECK (id = 1),
          value TEXT NOT NULL,
          expires_at INTEGER NOT NULL,
          retry_at INTEGER NOT NULL
        );
        `);
      });
    });
  }

  async fetch(request: Request): Promise<Response> {
    this.envVars = containerEnv(this.env);
    return super.fetch(request);
  }

  cacheGet(key: string): string | null {
    const row = this.ctx.storage.sql.exec<ModelCacheRow>(
      "SELECT value, expires_at FROM model_cache WHERE key = ?",
      key
    ).toArray()[0];
    if (!row) return null;
    if (row.expires_at > Date.now()) return row.value;
    this.ctx.storage.sql.exec("DELETE FROM model_cache WHERE key = ?", key);
    return null;
  }

  cacheKnown(key: string): boolean {
    return this.ctx.storage.sql.exec(
      "SELECT 1 FROM known_media WHERE key = ? AND seen_at > ? LIMIT 1",
      key, Date.now() - knownMediaTtlMs
    ).toArray().length > 0;
  }

  cachePut(key: string, value: string, expiresAt: number, known: boolean): void {
    this.ctx.storage.sql.exec(
      "INSERT INTO model_cache (key, value, expires_at) VALUES (?, ?, ?) " +
      "ON CONFLICT(key) DO UPDATE SET value = excluded.value, expires_at = excluded.expires_at",
      key, value, expiresAt
    );
    if (known) {
      this.ctx.storage.sql.exec(
        "INSERT INTO known_media(key, seen_at) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET seen_at = excluded.seen_at",
        key, Date.now()
      );
    }
    this.ctx.storage.sql.exec(
      "DELETE FROM model_cache WHERE key IN (SELECT key FROM model_cache WHERE expires_at <= ? LIMIT 100)",
      Date.now()
    );
    this.ctx.storage.sql.exec(
      "DELETE FROM known_media WHERE key IN (SELECT key FROM known_media WHERE seen_at <= ? LIMIT 100)",
      Date.now() - knownMediaTtlMs
    );
  }

  takeProxyBudget(limit: number): boolean {
    if (!Number.isSafeInteger(limit) || limit <= 0) return true;
    const bucket = Math.floor(Date.now() / 3_600_000);
    const allowed = this.ctx.storage.sql.exec(
      "INSERT INTO proxy_budget(bucket, used) VALUES (?, 1) " +
      "ON CONFLICT(bucket) DO UPDATE SET used = used + 1 WHERE used < ? RETURNING used",
      bucket, limit
    ).toArray().length > 0;
    this.ctx.storage.sql.exec("DELETE FROM proxy_budget WHERE bucket < ?", bucket - 1);
    return allowed;
  }

  getStatus(requestId: string): Promise<StatusResult> {
    const now = Date.now();
    const cached = this.ctx.storage.sql.exec<StatusCacheRow>(
      "SELECT value, expires_at, retry_at FROM status_cache WHERE id = 1"
    ).toArray()[0];
    const series = cached?.value ? parseStatusSeries(cached.value) : null;
    if (series && cached.expires_at > now) {
      return Promise.resolve({ series, status: 200, cacheSeconds: 60, stale: false });
    }
    if (cached?.retry_at > now) {
      return Promise.resolve({
        series: series ?? emptyStatusSeries(),
        status: series ? 200 : 502,
        cacheSeconds: 10,
        stale: Boolean(series)
      });
    }
    if (!this.statusRefresh) {
      this.statusRefresh = this.refreshStatus(cached, series, requestId).finally(() => {
        this.statusRefresh = undefined;
      });
    }
    return this.statusRefresh;
  }

  private async refreshStatus(cached: StatusCacheRow | undefined, stale: StatusSeries | null, requestId: string): Promise<StatusResult> {
    try {
      const series = await queryStatus(this.env, requestId);
      const expiresAt = Date.now() + 60_000;
      this.ctx.storage.sql.exec(
        "INSERT INTO status_cache(id, value, expires_at, retry_at) VALUES (1, ?, ?, 0) " +
        "ON CONFLICT(id) DO UPDATE SET value = excluded.value, expires_at = excluded.expires_at, retry_at = 0",
        JSON.stringify(series), expiresAt
      );
      return { series, status: 200, cacheSeconds: 60, stale: false };
    } catch (error) {
      const retryAt = Date.now() + 10_000;
      if (cached) {
        this.ctx.storage.sql.exec("UPDATE status_cache SET retry_at = ? WHERE id = 1", retryAt);
      } else {
        this.ctx.storage.sql.exec(
          "INSERT INTO status_cache(id, value, expires_at, retry_at) VALUES (1, '', 0, ?)", retryAt
        );
      }
      const detail = error instanceof Error ? error.message : String(error);
      console.error({ event: "status_query_failed", request_id: requestId, error: detail, stale: Boolean(stale) });
      return {
        series: stale ?? emptyStatusSeries(),
        status: stale ? 200 : 502,
        cacheSeconds: 10,
        stale: Boolean(stale)
      };
    }
  }
}

OgUsContainer.outboundByHost = { "cache.do": handleModelCache, "budget.do": handleProxyBudget };

async function handleModelCache(request: Request, env: Env, ctx: OutboundHandlerContext): Promise<Response> {
  const key = new URL(request.url).searchParams.get("key") ?? "";
  if (!modelCacheKeyRE.test(key)) return new Response(null, { status: 400 });

  const stub = env.OG_CONTAINER.get(env.OG_CONTAINER.idFromString(ctx.containerId));
  if (request.method === "HEAD") {
    return new Response(null, { status: await stub.cacheKnown(key) ? 204 : 404 });
  }
  if (request.method === "GET") {
    const value = await stub.cacheGet(key);
    return new Response(value, { status: value === null ? 404 : 200, headers: { "content-type": "application/json" } });
  }
  if (request.method === "PUT") {
    const expiresAt = Number(request.headers.get("x-cache-expires"));
    const value = await readTextBody(request, maxModelCacheBodyBytes);
    if (!Number.isSafeInteger(expiresAt) || expiresAt <= Date.now() || value === null) {
      return new Response(null, { status: 400 });
    }
    await stub.cachePut(key, value, expiresAt, request.headers.get("x-cache-known") === "1");
    return new Response(null, { status: 204 });
  }
  return new Response(null, { status: 405, headers: { allow: "HEAD, GET, PUT" } });
}

async function handleProxyBudget(request: Request, env: Env, ctx: OutboundHandlerContext): Promise<Response> {
  if (request.method !== "POST") return new Response(null, { status: 405, headers: { allow: "POST" } });
  const limit = Number(env.PROXY_HOURLY_LIMIT);
  if (!Number.isSafeInteger(limit)) return new Response(null, { status: 503 });
  const stub = env.OG_CONTAINER.get(env.OG_CONTAINER.idFromString(ctx.containerId));
  return new Response(null, { status: await stub.takeProxyBudget(limit) ? 204 : 429 });
}

const CONTAINER_NAME = "oginstagram-us";
const CONTAINER_HINT: DurableObjectLocationHint = "enam";

export default {
  async fetch(request, env, ctx): Promise<Response> {
    const started = Date.now();
    const url = new URL(request.url);
    const ray = request.headers.get("cf-ray");
    const requestId = ray && ray.length <= 128 ? ray : crypto.randomUUID();
    if (url.pathname === "/api/status") {
      return serveStatus(env, ctx, url, requestId);
    }
    if (url.pathname === "/api/embed") {
      const response = await serveEmbed(request, env, ctx, url, requestId);
      const log = response.status >= 500 ? console.error : response.status >= 400 ? console.warn : console.info;
      log({ event: "embed_request", request_id: requestId, method: request.method, status: response.status, ms: Date.now() - started });
      return response;
    }

    const meta: RequestMeta = { cacheHit: false, requestId };
    let response: Response;
    try {
      response = await handleAppRequest(request, env, ctx, url, meta);
    } catch (err) {
      meta.reason = "exception";
      logRequestMetric(request, url, meta, Date.now() - started, 500, false, env.AE);
      throw err;
    }
    if (response.status === 302 && url.pathname.startsWith("/offload/") && request.headers.get("sec-fetch-site") === "same-origin") {
      response = await proxyOffloadMedia(response, url.searchParams.get("preview") === "1", request.headers.get("accept") ?? "", requestId);
    }
    const metricStatus = meta.metricStatus ?? response.status;
    logRequestMetric(request, url, meta, Date.now() - started, metricStatus, metricStatus < 400, env.AE);
    return response;
  }
} satisfies ExportedHandler<Env>;

async function proxyOffloadMedia(redirect: Response, preview: boolean, accept: string, requestId: string): Promise<Response> {
  const target = redirect.headers.get("location") ?? "";
  let hostname = "";
  try {
    hostname = new URL(target).hostname;
  } catch {
    return redirect;
  }
  if (!hostname.endsWith(".cdninstagram.com") && !hostname.endsWith(".fbcdn.net")) {
    return redirect;
  }
  const signal = AbortSignal.timeout(10000);
  const original: RequestInitCfProperties = { cacheEverything: true, cacheTtl: 3600 };
  const transformed: RequestInitCfProperties = { ...original, image: { width: 720, fit: "scale-down" } };
  if (/image\/avif/.test(accept)) transformed.image!.format = "avif";
  else if (/image\/webp/.test(accept)) transformed.image!.format = "webp";
  const load = async (cf: RequestInitCfProperties, variant: "original" | "transformed") => {
    const started = Date.now();
    try {
      const response = await fetch(target, { signal, cf });
      logOutbound("media_proxy", target, requestId, started, response.status, { variant });
      return response.ok ? response : null;
    } catch (error) {
      logOutbound("media_proxy", target, requestId, started, 0, { variant, error: errorMessage(error) });
      return null;
    }
  };
  const media = await load(preview ? transformed : original, preview ? "transformed" : "original")
    ?? (preview ? await load(original, "original") : null);
  if (!media) return redirect;
  const out = new Response(media.body, { status: 200, headers: { "content-type": media.headers.get("content-type") ?? "application/octet-stream" } });
  out.headers.set("cross-origin-resource-policy", "cross-origin");
  out.headers.set("cache-control", "public, max-age=3600");
  if (preview) out.headers.set("vary", "accept");
  return out;
}

type RequestMeta = { cacheHit: boolean; requestId: string; metricStatus?: number; reason?: string; metric?: string };

type EmbedBody = { path: string; token: string };

type TurnstileResult = {
  success?: boolean;
  action?: string;
  hostname?: string;
  "error-codes"?: unknown;
};

async function serveEmbed(request: Request, env: Env, ctx: ExecutionContext, url: URL, requestId: string): Promise<Response> {
  if (request.method !== "POST") {
    return embedError(405, "method not allowed", { allow: "POST" });
  }
  if (request.headers.get("origin") !== url.origin || request.headers.get("sec-fetch-site") !== "same-origin") {
    return embedError(403, "forbidden");
  }
  if (!request.headers.get("content-type")?.toLowerCase().startsWith("application/json")) {
    return embedError(415, "application/json required");
  }

  const body = await readEmbedBody(request);
  if (!body || !validEmbedPath(body.path)) {
    return embedError(400, "invalid Instagram path");
  }

  // wrangler dev has no challenge platform, so no cf_clearance cookie can exist there.
  const rateKey = await clearanceRateKey(request)
    ?? (url.hostname === "localhost" || url.hostname === "127.0.0.1" ? "embed:clearance:dev" : null);
  if (!rateKey) {
    return embedError(403, "clearance required");
  }
  // Rate limit before siteverify so throttled requests never cost an external call.
  if (!(await env.EMBED_RATE_LIMITER.limit({ key: rateKey })).success) {
    return embedError(429, "rate limited", { "retry-after": "60" });
  }
  if (!(await verifyTurnstile(body.token, request, env, url, requestId))) {
    return embedError(403, "verification failed");
  }

  const target = new URL(body.path, url.origin);
  const headers = new Headers({ "user-agent": "OGInstagramPreviewBot/1.0" });
  const response = await handleAppRequest(new Request(target, { headers }), env, ctx, target, { cacheHit: false, requestId });
  const safe = new Response(response.body, response);
  safe.headers.set("cache-control", "no-store");
  safe.headers.set("x-content-type-options", "nosniff");
  return safe;
}

async function readEmbedBody(request: Request): Promise<EmbedBody | null> {
  const text = await readTextBody(request, maxEmbedBodyBytes);
  if (text === null) return null;
  try {
    const value = JSON.parse(text) as Partial<EmbedBody>;
    return typeof value.path === "string" && typeof value.token === "string"
      ? { path: value.path, token: value.token }
      : null;
  } catch {
    return null;
  }
}

// Cloudflare's public test secret always accepts its matching dummy token.
const dummyTurnstileSecret = "1x0000000000000000000000000000000AA";

async function verifyTurnstile(token: string, request: Request, env: Env, url: URL, requestId: string): Promise<boolean> {
  if (!env.TURNSTILE_SECRET_KEY || !token || token.length > 2048) return false;
  const form = new FormData();
  form.set("secret", env.TURNSTILE_SECRET_KEY);
  form.set("response", token);
  const ip = request.headers.get("cf-connecting-ip");
  if (ip) form.set("remoteip", ip);
  form.set("idempotency_key", crypto.randomUUID());

  for (let attempt = 0; attempt < 2; attempt++) {
    const started = Date.now();
    try {
      const response = await fetch("https://challenges.cloudflare.com/turnstile/v0/siteverify", {
        method: "POST",
        body: form,
        signal: AbortSignal.timeout(5000),
      });
      logOutbound("turnstile_siteverify", response.url, requestId, started, response.status, { attempt: attempt + 1 });
      if (!response.ok) {
        if (response.status >= 500 && attempt === 0) continue;
        return false;
      }
      const result = await response.json() as TurnstileResult;
      const testing = env.TURNSTILE_SECRET_KEY === dummyTurnstileSecret
        && (url.hostname === "localhost" || url.hostname === "127.0.0.1");
      const valid = result.success === true
        && (testing || (result.action === "turnstile-spin-v1" && result.hostname === url.hostname));
      if (!valid) console.warn({ event: "turnstile_rejected", request_id: requestId, action: result.action, hostname: result.hostname, errors: result["error-codes"] });
      return valid;
    } catch (error) {
      logOutbound("turnstile_siteverify", "https://challenges.cloudflare.com/turnstile/v0/siteverify", requestId, started, 0, { attempt: attempt + 1, error: errorMessage(error) });
      if (attempt === 0) continue;
      console.error({ event: "turnstile_unavailable", request_id: requestId, error: error instanceof Error ? error.message : "unknown" });
    }
  }
  return false;
}

async function clearanceRateKey(request: Request): Promise<string | null> {
  const clearance = request.headers.get("cookie")?.split(";").map((part) => part.trim()).find((part) => part.startsWith("cf_clearance="))?.slice(13);
  if (!clearance || clearance.length > 4096) return null;
  const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(clearance)));
  return `embed:clearance:${Array.from(digest, (byte) => byte.toString(16).padStart(2, "0")).join("")}`;
}

async function readTextBody(request: Request, maxBytes: number): Promise<string | null> {
  const reader = request.body?.getReader();
  if (!reader) return null;
  const decoder = new TextDecoder();
  let size = 0;
  let text = "";
  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    size += value.byteLength;
    if (size > maxBytes) {
      await reader.cancel();
      return null;
    }
    text += decoder.decode(value, { stream: true });
  }
  text += decoder.decode();
  return text;
}

function embedError(status: number, error: string, extraHeaders: Record<string, string> = {}): Response {
  return Response.json({ error }, {
    status,
    headers: { "cache-control": "no-store", "x-content-type-options": "nosniff", ...extraHeaders }
  });
}

async function serveHome(request: Request, env: Env, url: URL): Promise<Response> {
  const forced = asHomeLocale((url.searchParams.get("hl") ?? "").toLowerCase());
  const cookieLocale = asHomeLocale(request.headers.get("cookie")?.match(/(?:^|;\s*)hl=([\w-]+)/)?.[1] ?? "");
  const locale = forced ?? cookieLocale ?? resolveHomeLocale(request.headers.get("accept-language") ?? "");
  const asset = await env.ASSETS.fetch(new URL(`/home/${locale}.html`, url.origin));
  if (!asset.ok) return new Response("home page unavailable", { status: 500 });

  const base = env.BASE_URL || url.origin;
  const canonical = forced ? `${base}/?hl=${forced}` : `${base}/`;
  const nonce = crypto.randomUUID().replaceAll("-", "");
  const html = (await asset.text())
    .replaceAll("__OG_CANONICAL__", canonical)
    .replaceAll("__OG_BASE__", base)
    .replaceAll("__OG_HOST__", url.host)
    .replaceAll("__OG_TURNSTILE_SITE_KEY__", env.TURNSTILE_SITE_KEY ?? "")
    .replaceAll("__OG_CSP_NONCE__", nonce)
    .replaceAll("__OG_VERSION__", versionCode(env));

  const headers = new Headers({
    "content-type": "text/html; charset=utf-8",
    "cache-control": "public, max-age=0, must-revalidate",
    "vary": "Accept-Language, Cookie",
    "content-security-policy": `default-src 'self'; script-src 'self' 'nonce-${nonce}' https://challenges.cloudflare.com; style-src 'self' 'unsafe-inline' https://cdn.jsdelivr.net; img-src 'self' data: https://*.cdninstagram.com https://*.fbcdn.net; font-src 'self' https://cdn.jsdelivr.net; connect-src 'self' https://challenges.cloudflare.com; frame-src https://challenges.cloudflare.com; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'`,
    "permissions-policy": "camera=(), geolocation=(), microphone=()",
    "referrer-policy": "strict-origin-when-cross-origin",
    "x-content-type-options": "nosniff",
    "x-frame-options": "DENY",
  });
  if (forced && forced !== cookieLocale) {
    headers.append("set-cookie", `hl=${forced}; Path=/; Max-Age=31536000; SameSite=Lax`);
  }
  return new Response(html, { headers });
}

async function handleAppRequest(request: Request, env: Env, ctx: ExecutionContext, url: URL, meta: RequestMeta): Promise<Response> {
  if (url.pathname === "/") {
    return serveHome(request, env, url);
  }
  const route = resolveContainerRoute(url);
  if (!route) {
    return new Response(null, { status: 404 });
  }
  meta.metric = route.metric;

  if (route.humanRedirect && !botRE.test(request.headers.get("user-agent") ?? "")) {
    return Response.redirect(route.humanRedirect, 307);
  }

  const cacheKey = edgeCacheKey(route, request, url);
  const cacheableMethod = request.method === "GET";
  const hit = cacheableMethod ? await defaultCache.match(cacheKey) : undefined;
  if (hit) {
    meta.cacheHit = true;
    return hit;
  }

  const routedRequest = route.rewritePath
    ? new Request(new URL(route.rewritePath, url.origin).href, request)
    : request;
  const headers = new Headers(routedRequest.headers);
  headers.set("og-public-origin", url.origin);
  headers.set("og-request-id", meta.requestId);
  // Only the edge may authorize uncached offload fetches.
  headers.delete("og-allow-offload-fetch");
  if (route.allowOffloadFetch) {
    headers.set("og-allow-offload-fetch", "1");
  }
  const containerRequest = new Request(routedRequest, { headers });
  let response = await env.OG_CONTAINER.get(
    env.OG_CONTAINER.idFromName(CONTAINER_NAME),
    { locationHint: CONTAINER_HINT }
  ).fetch(containerRequest);

  if (response.headers.get("og-cache") === "hit") {
    meta.cacheHit = true;
  }

  // Error cards stay HTTP 200; OG fields preserve the real outcome for metrics.
  const ogStatus = response.headers.get("og-status");
  if (ogStatus) {
    meta.metricStatus = Number.parseInt(ogStatus, 10) || undefined;
  }

  const ogReason = response.headers.get("og-reason");
  if (ogReason) {
    meta.reason = ogReason;
  }

  if (
    response.headers.has("og-cache") ||
    response.headers.has("og-status") ||
    response.headers.has("og-reason")
  ) {
    response = new Response(response.body, response);
    response.headers.delete("og-cache");
    response.headers.delete("og-status");
    response.headers.delete("og-reason");
  }

  if (cacheableMethod && response.headers.get("Cache-Control")?.includes("s-maxage")) {
    ctx.waitUntil(defaultCache.put(cacheKey, response.clone()));
  }
  return response;
}

function containerEnv(env: Env): Record<string, string> {
  return {
    PROXY_USERNAME: env.PROXY_USERNAME ?? "",
    PROXY_PASSWORD: env.PROXY_PASSWORD ?? "",
    BASE_URL: env.BASE_URL ?? "",
    MODEL_CACHE_URL: "http://cache.do",
    BUDGET_URL: "http://budget.do",
    OG_VERSION: versionCode(env)
  };
}

function logRequestMetric(
  request: Request,
  url: URL,
  meta: RequestMeta,
  ms: number,
  status: number,
  ok: boolean,
  ae?: AnalyticsEngineDataset
): void {
  const route = meta.metric;
  if (!route) {
    return;
  }
  const client = clientClass(request.headers.get("user-agent") ?? "");
  if (client === "human") {
    return;
  }
  const outcome = ok ? "ok" : "fail";
  const cache = meta.cacheHit ? "hit" : "miss";
  const reasonBlob = meta.reason || (ok ? "ok" : "fail");
  const message = `${request.method} ${url.pathname} ${status} ${ms}ms cache=${cache} client=${client}${ok ? "" : ` reason=${reasonBlob}`}`;
  const log = status >= 500 ? console.error : status >= 400 ? console.warn : console.info;
  log({ message, event: "http_request", request_id: meta.requestId, route, client, outcome, status, ms, cache, reason: reasonBlob, path: url.pathname });
  if (meta.cacheHit) {
    return;
  }
  ae?.writeDataPoint({
    blobs: [route, client, outcome, reasonBlob],
    doubles: [ms, ok ? 1 : 0, status],
    indexes: [route]
  });
}

function versionCode(env: Env): string {
  return env.CF_VERSION_METADATA?.id?.replaceAll("-", "").slice(0, 8) || "dev";
}

type ContainerRoute = {
  cacheKey: string;
  allowOffloadFetch?: boolean;
  varyBot?: boolean;
  humanRedirect?: string;
  rewritePath?: string;
  metric?: "embed" | "direct" | "profile";
};

function isDirectHost(url: URL): boolean {
  return url.hostname.startsWith("d.") || url.hostname.startsWith("www.d.");
}

function isGalleryHost(url: URL): boolean {
  return url.hostname.startsWith("g.") || url.hostname.startsWith("www.g.");
}

function resolveContainerRoute(url: URL): ContainerRoute | null {
  const path = url.pathname;
  const segments = splitPath(path);
  if (path === "/.well-known/webfinger") {
    return { cacheKey: path + url.search };
  }
  if (segments[0] === "offload" && (
    segments.length === 2 ||
    segments.length === 3 ||
    (segments[1] === "story" && (segments.length === 4 || (segments.length === 5 && segments[4] === "avatar")))
  )) {
    const thumbnail = url.searchParams.has("thumbnail") ? "?thumbnail=1" : "";
    return { cacheKey: `${path}${thumbnail}` };
  }
  const stories = parseStoriesSegments(segments);
  if (stories) {
    const username = encodeURIComponent(stories.username);
    if (isDirectHost(url)) {
      return {
        cacheKey: `/direct/story/${username}/${stories.storyID}`,
        rewritePath: `/offload/story/${username}/${stories.storyID}`,
        allowOffloadFetch: true,
        metric: "direct"
      };
    }
    const gallery = isGalleryHost(url);
    return {
      cacheKey: `/${gallery ? "gallery" : "embed"}/story/${username}/${stories.storyID}`,
      varyBot: true,
      humanRedirect: `${instagramOrigin}/stories/${username}/${stories.storyID}/`,
      rewritePath: gallery ? galleryRewritePath(url) : undefined,
      metric: "embed"
    };
  }
  if (
    (segments.length === 4 && segments[0] === "api" && segments[1] === "v1" && segments[2] === "statuses") ||
    (segments.length === 4 && segments[0] === "users" && segments[2] === "statuses")
  ) {
    return { cacheKey: path };
  }
  if (segments.length === 3 && segments[0] === "users" && validUsername(segments[1]) && (segments[2] === "inbox" || segments[2] === "outbox")) {
    return { cacheKey: path };
  }
  if (segments.length === 2 && segments[0] === "users" && validUsername(segments[1])) {
    return { cacheKey: path };
  }

  const embed = parseEmbedSegments(segments);
  if (embed) {
    const selected = mediaSelection(url.searchParams, embed.pathIndex);
    if (isDirectHost(url)) {
      return {
        cacheKey: `/direct/${encodeURIComponent(embed.shortcode)}/${selected ?? "-"}`,
        rewritePath: `/offload/${encodeURIComponent(embed.shortcode)}/${(selected ?? 0) + 1}`,
        allowOffloadFetch: true,
        metric: "direct"
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
      rewritePath: gallery ? galleryRewritePath(url) : undefined,
      metric: "embed"
    };
  }

  if (segments.length === 1 && validUsername(segments[0])) {
    const username = segments[0];
    const gallery = isGalleryHost(url);
    return {
      cacheKey: `/${gallery ? "pgallery" : "profile"}/${encodeURIComponent(username)}`,
      varyBot: true,
      humanRedirect: `${instagramOrigin}/${encodeURIComponent(username)}/`,
      rewritePath: gallery ? galleryRewritePath(url) : undefined,
      metric: "profile"
    };
  }
  return null;
}

function galleryRewritePath(url: URL): string {
  const rewritten = new URL(url.pathname + url.search, url.origin);
  rewritten.searchParams.set("__gallery", "1");
  return rewritten.pathname + rewritten.search;
}

function edgeCacheKey(route: ContainerRoute, request: Request, url: URL): string {
  let key = `${url.origin}/__edge${route.cacheKey}`;
  if (route.varyBot) {
    const telegram = /telegrambot/i.test(request.headers.get("user-agent") ?? "");
    key += `${key.includes("?") ? "&" : "?"}bot=${telegram ? "telegram" : "other"}`;
  }
  return key;
}

function clientClass(userAgent: string): string {
  if (!botRE.test(userAgent)) {
    return "human";
  }
  if (/discordbot/i.test(userAgent)) {
    return "discord";
  }
  if (/telegrambot/i.test(userAgent)) {
    return "telegram";
  }
  return "bot";
}

const STATUS_QUERY =
  "SELECT intDiv(toUInt32(timestamp), 600) * 600 AS t, " +
  "sum(_sample_interval * double1) / sum(_sample_interval) AS latency, " +
  "sumIf(_sample_interval, blob3 = 'ok') AS resolved, " +
  "sumIf(_sample_interval, blob3 = 'fail' AND blob4 = 'GeoBlockRequired') AS restricted, " +
  "sumIf(_sample_interval, blob3 = 'fail' AND blob4 != 'GeoBlockRequired') AS failed " +
  "FROM oginstagram_requests WHERE timestamp > NOW() - INTERVAL '1' DAY GROUP BY t ORDER BY t";

type StatusSeries = {
  t: number[];
  latency: number[];
  resolved: number[];
  restricted: number[];
  failed: number[];
};

type StatusRow = { t: unknown; latency: unknown; resolved: unknown; restricted: unknown; failed: unknown };

async function serveStatus(env: Env, ctx: ExecutionContext, url: URL, requestId: string): Promise<Response> {
  const cacheKey = `${url.origin}/__status`;
  const hit = await defaultCache.match(cacheKey);
  if (hit) {
    return hit;
  }
  if (!env.AE_ACCOUNT_ID || !env.AE_API_TOKEN) {
    return statusJSON(emptyStatusSeries(), 503);
  }
  const stub = env.OG_CONTAINER.get(env.OG_CONTAINER.idFromName(CONTAINER_NAME), { locationHint: CONTAINER_HINT });
  const result = await stub.getStatus(requestId);
  const response = statusJSON(result.series, result.status, `public, s-maxage=${result.cacheSeconds}`);
  if (result.stale) response.headers.set("warning", '110 - "Response is stale"');
  ctx.waitUntil(defaultCache.put(cacheKey, response.clone()));
  return response;
}

async function queryStatus(env: Env, requestId: string): Promise<StatusSeries> {
  if (!env.AE_ACCOUNT_ID || !env.AE_API_TOKEN) throw new Error("analytics credentials unavailable");
  const target = `https://api.cloudflare.com/client/v4/accounts/${env.AE_ACCOUNT_ID}/analytics_engine/sql`;
  const started = Date.now();
  let upstream: Response;
  try {
    upstream = await fetch(target, {
      method: "POST",
      headers: { Authorization: `Bearer ${env.AE_API_TOKEN}` },
      body: `${STATUS_QUERY} FORMAT JSON`,
      signal: AbortSignal.timeout(5000)
    });
  } catch (error) {
    logOutbound("analytics_query", target, requestId, started, 0, { error: errorMessage(error) });
    throw error;
  }
  logOutbound("analytics_query", target, requestId, started, upstream.status);
  if (!upstream.ok) throw new Error(`analytics upstream ${upstream.status}`);
  const parsed = (await upstream.json()) as { data?: StatusRow[] };
  const rows = parsed.data ?? [];
  return {
    t: rows.map(row => Number(row.t)),
    latency: rows.map(row => Math.round(Number(row.latency) * 100) / 100),
    resolved: rows.map(row => Math.round(Number(row.resolved) * 100) / 100),
    restricted: rows.map(row => Math.round(Number(row.restricted) * 100) / 100),
    failed: rows.map(row => Math.round(Number(row.failed) * 100) / 100)
  };
}

function parseStatusSeries(value: string): StatusSeries | null {
  try {
    const parsed = JSON.parse(value) as StatusSeries;
    return Array.isArray(parsed.t) && Array.isArray(parsed.latency) && Array.isArray(parsed.resolved)
      && Array.isArray(parsed.restricted) && Array.isArray(parsed.failed) ? parsed : null;
  } catch {
    return null;
  }
}

function emptyStatusSeries(): StatusSeries {
  return { t: [], latency: [], resolved: [], restricted: [], failed: [] };
}

function statusJSON(series: StatusSeries, status: number, cacheControl?: string): Response {
  return Response.json(series, {
    status,
    headers: cacheControl ? { "cache-control": cacheControl } : undefined
  });
}

function logOutbound(
  operation: string,
  target: string,
  requestId: string,
  started: number,
  status: number,
  fields: Record<string, unknown> = {}
): void {
  const url = new URL(target);
  const entry = {
    event: "outbound_request",
    request_id: requestId,
    operation,
    host: url.hostname,
    path: url.pathname,
    status,
    ms: Date.now() - started,
    ...fields
  };
  const log = status === 0 || status >= 500 ? console.error : status >= 400 ? console.warn : console.info;
  log(entry);
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
