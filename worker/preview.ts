import { edgeCacheKey, resolveContainerRoute, validEmbedPath } from "../shared/routes";
import {
  internalHeader,
  problemResponse,
  publicEmbedStatus,
  readTextBody,
  stampGatewayHeaders,
  stripInternalHeaders,
} from "./http";
import { verifyTurnstile } from "./turnstile";

const maxEmbedBodyBytes = 4096;

type EmbedBody = { path: string; token: string };
type TimingSafeSubtleCrypto = SubtleCrypto & {
  timingSafeEqual(a: ArrayBuffer | ArrayBufferView, b: ArrayBuffer | ArrayBufferView): boolean;
};

export async function serveEmbed(
  request: Request,
  env: Env,
  ctx: ExecutionContext,
  url: URL,
  requestId: string
): Promise<Response> {
  if (request.method !== "POST") return problemResponse(405, "method not allowed", { allow: "POST" });
  if (request.headers.get("origin") !== url.origin || request.headers.get("sec-fetch-site") !== "same-origin") {
    return problemResponse(403, "forbidden");
  }
  if (!request.headers.get("content-type")?.toLowerCase().startsWith("application/json")) {
    return problemResponse(415, "application/json required");
  }

  const body = await readEmbedBody(request);
  if (!body || !validEmbedPath(body.path)) return problemResponse(400, "invalid Instagram path");

  const target = new URL(body.path, url.origin);
  if (target.origin !== url.origin) return problemResponse(400, "invalid Instagram path");

  // wrangler dev has no challenge platform, so no cf_clearance cookie exists.
  const rateKey = await clearanceRateKey(request)
    ?? (url.hostname === "localhost" || url.hostname === "127.0.0.1" ? "embed:clearance:dev" : null);
  if (!rateKey) return problemResponse(403, "clearance required");
  if (!(await env.EMBED_RATE_LIMITER.limit({ key: rateKey })).success) {
    return problemResponse(429, "rate limited", { "retry-after": "60" });
  }
  const verification = await verifyTurnstile(body.token, request, env, url);
  if (verification.outcome === "unavailable") {
    const entry: Record<string, unknown> = {
      event: "turnstile_unavailable",
      request_id: requestId,
      "error.type": verification.errorType,
      "exception.message": verification.message,
    };
    if (verification.httpStatus !== undefined) {
      entry["http.response.status_code"] = verification.httpStatus;
    }
    console.error(entry);
    return problemResponse(503, "verification service unavailable", { "retry-after": "1" });
  }
  if (verification.outcome === "rejected") {
    return problemResponse(403, "verification failed");
  }

  const response = await fetchPreviewHTML(ctx, url, target, requestId);
  const publicStatus = publicEmbedStatus(Number(response.headers.get(internalHeader.originStatus)));
  if (publicStatus !== null) return problemResponse(publicStatus, "preview unavailable");
  const stripped = stripInternalHeaders(response);
  const safe = new Response(stripped.body, stripped);
  safe.headers.set("cache-control", "no-store");
  safe.headers.delete("cloudflare-cdn-cache-control");
  safe.headers.set("x-content-type-options", "nosniff");
  return safe;
}

export async function isValidAdminToken(request: Request, env: Env): Promise<boolean> {
  const provided = request.headers.get("authorization")?.match(/^Bearer\s+(.+)$/i)?.[1];
  if (!provided || !env.ADMIN_PURGE_TOKEN) return false;
  const [actual, expected] = await Promise.all([sha256(provided), sha256(env.ADMIN_PURGE_TOKEN)]);
  return (crypto.subtle as TimingSafeSubtleCrypto).timingSafeEqual(actual, expected);
}

async function fetchPreviewHTML(
  ctx: ExecutionContext,
  publicUrl: URL,
  target: URL,
  requestId: string
): Promise<Response> {
  const synthetic = stampGatewayHeaders(
    new Request(target, { headers: { "user-agent": "OGInstagramPreviewBot/1.0" } }),
    publicUrl,
    requestId
  );
  const route = resolveContainerRoute(target);
  if (!route) return problemResponse(400, "invalid Instagram path");
  return ctx.exports.Embed.fetch(synthetic, { cf: { cacheKey: edgeCacheKey(route, target) } });
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

async function clearanceRateKey(request: Request): Promise<string | null> {
  // Cloudflare's Managed Challenge validates this opaque cookie before the
  // request reaches the Worker. Here it is only a rate-limit characteristic;
  // Workers does not expose a separate cf_clearance verification API.
  const clearanceCookies = (request.headers.get("cookie") ?? "")
    .split(";")
    .map((part) => part.trim())
    .filter((part) => part.startsWith("cf_clearance="));
  if (clearanceCookies.length !== 1) return null;
  const clearance = clearanceCookies[0].slice("cf_clearance=".length);
  if (!clearance || clearance.length > 4096) return null;
  return `embed:clearance:${await sha256Hex(clearance)}`;
}

async function sha256(value: string): Promise<Uint8Array> {
  return new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(value)));
}

async function sha256Hex(value: string): Promise<string> {
  const digest = await sha256(value);
  return Array.from(digest, (byte) => byte.toString(16).padStart(2, "0")).join("");
}
