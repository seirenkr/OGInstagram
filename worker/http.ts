// Shared HTTP boundary helpers. Keeping internal headers and body limits here
// makes it harder for a new public route to accidentally trust client-supplied
// internal metadata or return an implicitly cacheable response.

// The whole bot-facing cycle — request received at the edge to response
// returned — must fit in this budget. The container applies the same flat
// timeout to itself, so this is only the edge's own allowance.
export const overallBudgetMs = 4500;

export const internalHeader = {
  allowOriginFetch: "oginstagram-allow-origin-fetch",
  cacheStatus: "oginstagram-cache-status",
  errorType: "oginstagram-error-type",
  originDuration: "oginstagram-origin-duration-ms",
  originStatus: "oginstagram-origin-status",
  publicOrigin: "oginstagram-public-origin",
  requestId: "oginstagram-request-id",
  signedOffload: "oginstagram-signed-offload",
} as const;

export function stampGatewayHeaders(
  request: Request,
  publicUrl: URL,
  requestId: string,
  options: { signedOffload?: boolean } = {}
): Request {
  const headers = new Headers(request.headers);
  // Every internal header a handler trusts is cleared here, so no route can
  // inherit one from the client by being reachable.
  headers.delete(internalHeader.allowOriginFetch);
  headers.delete(internalHeader.signedOffload);
  const origin = new URL(publicUrl);
  if (origin.hostname !== "localhost" && origin.hostname !== "127.0.0.1") origin.protocol = "https:";
  headers.set(internalHeader.publicOrigin, origin.origin);
  headers.set(internalHeader.requestId, requestId);
  if (options.signedOffload) headers.set(internalHeader.signedOffload, "1");
  return new Request(request, { headers });
}

export function ensureCacheControl(response: Response): Response {
  if (response.headers.has("cache-control") || response.headers.has("cloudflare-cdn-cache-control")) {
    return response;
  }
  const patched = new Response(response.body, response);
  patched.headers.set("cache-control", "no-store");
  return patched;
}

export function stripInternalHeaders(response: Response): Response {
  const internal = [
    internalHeader.cacheStatus,
    internalHeader.originStatus,
    internalHeader.errorType,
    internalHeader.originDuration,
  ];
  if (!internal.some((name) => response.headers.has(name))) return response;
  const stripped = new Response(response.body, response);
  for (const name of internal) stripped.headers.delete(name);
  return stripped;
}

export async function readTextBody(request: Request, maxBytes: number): Promise<string | null> {
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
  return text + decoder.decode();
}

export function versionCode(env: Env): string {
  return env.CF_VERSION_METADATA?.id?.replaceAll("-", "").slice(0, 8) || "dev";
}

export function problemResponse(
  status: number,
  detail: string,
  headers: Record<string, string> = {},
  extensions: Record<string, unknown> = {}
): Response {
  return Response.json({
    ...extensions,
    type: "about:blank",
    title: problemTitle(status),
    status,
    detail,
  }, {
    status,
    headers: {
      "cache-control": "no-store",
      "content-type": "application/problem+json",
      "x-content-type-options": "nosniff",
      ...headers,
    },
  });
}

export function publicEmbedStatus(status: number): number | null {
  if (!Number.isInteger(status) || status < 400 || status > 599) return null;
  return status === 429 ? 502 : status === 499 ? 504 : status;
}

function problemTitle(status: number): string {
  switch (status) {
    case 400: return "Bad Request";
    case 403: return "Forbidden";
    case 405: return "Method Not Allowed";
    case 415: return "Unsupported Media Type";
    case 429: return "Too Many Requests";
    case 502: return "Bad Gateway";
    case 503: return "Service Unavailable";
    case 504: return "Gateway Timeout";
    default: return "Request Failed";
  }
}
