import { validCacheKey } from "../shared/routes";
import { readTextBody } from "./http";

const maxCacheValueBytes = 1_900_000;

type CacheEntryMeta = { expiresAt: number };

export async function handleCache(
  request: Request,
  env: Env
): Promise<Response> {
  const key = new URL(request.url).searchParams.get("key") ?? "";
  if (!validCacheKey(key)) return new Response(null, { status: 400 });

  if (request.method === "GET") {
    const { value, metadata } = await env.CACHE_KV.getWithMetadata<CacheEntryMeta>(key, "text");
    if (value === null || !validMetadata(metadata)) {
      return new Response(null, { status: 404 });
    }
    return new Response(value, {
      status: 200,
      headers: {
        "content-type": "application/json",
        // The Go L1 tier needs the logical model expiry, not the later physical
        // KV deletion time, to warm safely after a remote hit.
        "x-cache-expires": String(metadata.expiresAt),
      },
    });
  }

  if (request.method === "PUT") {
    const expiresAt = Number(request.headers.get("x-cache-expires"));
    const value = await readTextBody(request, maxCacheValueBytes);
    if (!Number.isSafeInteger(expiresAt) || expiresAt <= Date.now() || value === null) {
      return new Response(null, { status: 400 });
    }
    try {
      await env.CACHE_KV.put(key, value, {
        // Workers KV requires at least 60 seconds. Do not retain a second,
        // unusable stale window: /offload must refresh an expired CDN URL from
        // Instagram rather than replay the expired model.
        expirationTtl: Math.max(60, Math.ceil((expiresAt - Date.now()) / 1000)),
        metadata: { expiresAt } satisfies CacheEntryMeta,
      });
    } catch (error) {
      // Workers KV permits one write per second per key. Concurrent fills are
      // equivalent, so a rate-limited duplicate does not fail the response.
      if (!String(error).includes("KV PUT failed: 429 Too Many Requests")) throw error;
    }
    return new Response(null, { status: 204 });
  }

  return new Response(null, { status: 405, headers: { allow: "GET, PUT" } });
}

function validMetadata(metadata: CacheEntryMeta | null): metadata is CacheEntryMeta {
  return metadata !== null
    && Number.isSafeInteger(metadata.expiresAt)
    && metadata.expiresAt > Date.now();
}
