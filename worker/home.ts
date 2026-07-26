import { asHomeLocale, resolveHomeLocale } from "../shared/routes";
import { versionCode } from "./http";

export function serviceBaseUrl(configured: string | undefined, requestUrl: URL): URL {
  try {
    return new URL(configured || requestUrl.origin);
  } catch {
    return new URL(requestUrl.origin);
  }
}

export async function serveHome(request: Request, env: Env, url: URL): Promise<Response> {
  const base = serviceBaseUrl(env.BASE_URL, url);
  if (!isLocalDevelopment(url) && url.origin !== base.origin) {
    const target = new URL("/", base);
    const language = url.searchParams.get("hl");
    if (language) target.searchParams.set("hl", language);
    return Response.redirect(target, 308);
  }

  const forced = asHomeLocale((url.searchParams.get("hl") ?? "").toLowerCase());
  const cookieLocale = asHomeLocale(request.headers.get("cookie")?.match(/(?:^|;\s*)hl=([\w-]+)/)?.[1] ?? "");
  const locale = forced ?? cookieLocale ?? resolveHomeLocale(request.headers.get("accept-language") ?? "");
  const asset = await env.ASSETS.fetch(new URL(`/home/${locale}.html`, url.origin));
  if (!asset.ok) {
    return new Response("home page unavailable", { status: 500, headers: { "cache-control": "no-store" } });
  }

  const canonical = forced ? `${base.origin}/?hl=${forced}` : `${base.origin}/`;
  const nonce = crypto.randomUUID().replaceAll("-", "");
  const html = (await asset.text())
    .replaceAll("__OG_CANONICAL__", canonical)
    .replaceAll("__OG_BASE__", base.origin)
    .replaceAll("__OG_HOST__", base.host)
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

function isLocalDevelopment(url: URL): boolean {
  return url.hostname === "localhost" || url.hostname === "127.0.0.1";
}
