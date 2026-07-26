const maxMediaRedirects = 3;

export async function proxyOffloadMedia(
  redirect: Response,
  preview: "media" | "avatar" | null,
  accept: string,
  requestId: string
): Promise<Response> {
  const target = redirect.headers.get("location") ?? "";
  let targetUrl: URL;
  try {
    targetUrl = new URL(target);
  } catch {
    logBlockedMedia(requestId, "invalid_redirect_url");
    return redirect;
  }
  if (!isTrustedMediaUrl(targetUrl)) {
    logBlockedMedia(requestId, "untrusted_redirect_destination");
    return redirect;
  }

  const original: RequestInitCfProperties = { cacheEverything: true, cacheTtl: 3600 };
  const transformed: RequestInitCfProperties = {
    ...original,
    image: { width: preview === "avatar" ? 96 : 720, fit: "scale-down" },
  };
  if (accept.includes("image/avif")) transformed.image!.format = "avif";
  else if (accept.includes("image/webp")) transformed.image!.format = "webp";

  const load = async (cf: RequestInitCfProperties) => {
    let current = targetUrl;
    for (let hop = 0; hop <= maxMediaRedirects; hop++) {
      try {
        const response = await fetch(current, { cf, redirect: "manual" });
        if (response.status >= 300 && response.status < 400) {
          const location = response.headers.get("location");
          await response.body?.cancel();
          if (!location || hop === maxMediaRedirects) return null;
          let next: URL;
          try {
            next = new URL(location, current);
          } catch {
            return null;
          }
          // Validate every destination before issuing the next subrequest.
          // A trusted CDN redirect must not turn this into an arbitrary
          // cross-origin fetch, even transiently.
          if (!isTrustedMediaUrl(next)) {
            logBlockedMedia(requestId, "untrusted_redirect_destination");
            return null;
          }
          current = next;
          continue;
        }
        let finalUrl: URL;
        try {
          finalUrl = new URL(response.url || current.href);
        } catch {
          return null;
        }
        return response.ok && isTrustedMediaUrl(finalUrl) ? response : null;
      } catch {
        return null;
      }
    }
    return null;
  };

  const media = await load(preview ? transformed : original)
    ?? (preview ? await load(original) : null);
  if (!media) return redirect;

  const output = new Response(media.body, {
    status: 200,
    headers: { "content-type": media.headers.get("content-type") ?? "application/octet-stream" },
  });
  output.headers.set("cross-origin-resource-policy", "cross-origin");
  output.headers.set("cache-control", "public, max-age=3600");
  if (preview) output.headers.set("vary", "accept");
  return output;
}

function logBlockedMedia(requestId: string, errorType: string): void {
  console.warn({
    event: "media_redirect_blocked",
    request_id: requestId,
    "error.type": errorType,
  });
}

function isTrustedMediaUrl(url: URL): boolean {
  const host = url.hostname.toLowerCase().replace(/\.$/, "");
  const trustedHost = host === "cdninstagram.com"
    || host.endsWith(".cdninstagram.com")
    || host === "fbcdn.net"
    || host.endsWith(".fbcdn.net");
  return url.protocol === "https:" && !url.username && !url.password
    && !url.port && trustedHost;
}
