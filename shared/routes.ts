export const botPattern =
  /bot|facebook|whatsapp|embed|got|firefox\/92|curl|wget|go-http|yahoo|generator|revoltchat|preview|link|proxy|vkshare|images|analyzer|index|crawl|spider|python|node|deno|mastodon|http\.rb|ruby|bun\/|fiddler|iframely|bluesky|matrix|cardyb|resolver|feedly|rss|reader|atom|thunderbird|axios/i;

type EmbedRoute = {
  postType: string;
  shortcode: string;
  pathIndex: number | null;
};

const shortcodePattern = /^[A-Za-z0-9_-]{1,24}$/;

function validShortcode(value: string): boolean {
  return shortcodePattern.test(value);
}

const usernamePattern = /^[A-Za-z0-9._]{1,30}$/;

const storyIdPattern = /^[0-9]{1,32}$/;

const cacheKeyPattern = /^(?:post:[A-Za-z0-9_-]{1,24}|profile:[A-Za-z0-9._]{1,30}|story:[A-Za-z0-9._]{1,30}\/[0-9]{1,32})$/;

const maxPostMediaItems = 50;
const maxProfileMediaItems = 6;

export function validCacheKey(value: string): boolean {
  return cacheKeyPattern.test(value);
}

function validUsername(value: string): boolean {
  return usernamePattern.test(value);
}

function parseStoriesSegments(segments: string[]): { username: string; storyId: string } | null {
  if (segments.length === 3 && segments[0] === "stories" && validUsername(segments[1]) && storyIdPattern.test(segments[2])) {
    return { username: segments[1], storyId: segments[2] };
  }
  return null;
}

function parseEmbedSegments(segments: string[]): EmbedRoute | null {
  if ((segments.length === 2 || segments.length === 3) && isPostRouteType(segments[0]) && validShortcode(segments[1])) {
    const pathIndex = optionalPathIndex(segments, 2);
    if (pathIndex === undefined) {
      return null;
    }
    return { postType: normalizePostType(segments[0]), shortcode: segments[1], pathIndex };
  }
  if ((segments.length === 3 || segments.length === 4) && isPostRouteType(segments[1]) && validShortcode(segments[2])) {
    const pathIndex = optionalPathIndex(segments, 3);
    if (pathIndex === undefined) {
      return null;
    }
    return { postType: normalizePostType(segments[1]), shortcode: segments[2], pathIndex };
  }
  return null;
}

function normalizePostType(value: string): string {
  return value === "reel" || value === "reels" ? "reel" : "p";
}

function isPostRouteType(value: string): boolean {
  return value === "p" || value === "reel" || value === "reels";
}

function optionalPathIndex(segments: string[], index: number): number | null | undefined {
  if (segments.length <= index) {
    return null;
  }
  return parseCanonicalDecimal(segments[index]) ?? undefined;
}

export function parseCanonicalDecimal(value: string): number | null {
  if (!/^(?:0|[1-9]\d*)$/.test(value)) {
    return null;
  }
  const parsed = Number(value);
  return Number.isSafeInteger(parsed) ? parsed : null;
}

export function mediaSelection(params: URLSearchParams, pathIndex: number | null): number | null {
  if (pathIndex !== null) return boundedMediaIndex(pathIndex - 1);
  for (const [key, oneBased] of [["img_index", true], ["index", false], ["order", false]] as const) {
    if (!params.has(key)) continue;
    const parsed = parseCanonicalDecimal(params.get(key) ?? "") ?? 0;
    return boundedMediaIndex(oneBased ? parsed - 1 : parsed);
  }
  return null;
}

function boundedMediaIndex(value: number): number {
  return Math.min(maxPostMediaItems - 1, Math.max(0, value));
}

type HomeLocale = "en" | "ja" | "ko" | "zh-hant" | "zh-hans" | "es" | "pt" | "fr";

const HOME_LOCALES: readonly string[] = ["en", "es", "fr", "ja", "ko", "pt", "zh-hans", "zh-hant"];

export function resolveHomeLocale(acceptLanguage: string): HomeLocale {
  const ranges = acceptLanguage.split(",").map((part, order) => {
    const [rawTag, ...parameters] = part.trim().split(";");
    let quality = 1;
    for (const parameter of parameters) {
      const match = /^\s*q\s*=\s*(0(?:\.\d{0,3})?|1(?:\.0{0,3})?)\s*$/i.exec(parameter);
      if (match) quality = Number(match[1]);
      else if (/^\s*q\s*=/i.test(parameter)) quality = 0;
    }
    return { tag: rawTag.toLowerCase(), quality, order };
  }).filter(({ tag, quality }) => tag && tag !== "*" && quality > 0)
    .sort((a, b) => b.quality - a.quality || a.order - b.order);

  for (const { tag } of ranges) {
    const matched = matchLocale(tag);
    if (matched) {
      return matched;
    }
  }
  return "en";
}

function matchLocale(tag: string): HomeLocale | null {
  if (tag === "zh" || tag.startsWith("zh-")) {
    return tag.includes("hant") || tag.endsWith("-tw") || tag.endsWith("-hk") || tag.endsWith("-mo") ? "zh-hant" : "zh-hans";
  }
  return asHomeLocale(tag.split("-")[0]);
}

export function asHomeLocale(value: string): HomeLocale | null {
  return HOME_LOCALES.includes(value) ? (value as HomeLocale) : null;
}

function splitPath(path: string): string[] {
  const trimmed = path.replace(/^\/+|\/+$/g, "");
  if (!trimmed) {
    return [];
  }
  return trimmed.split("/").map(segment => {
    try {
      return decodeURIComponent(segment);
    } catch {
      return segment;
    }
  });
}

export function validEmbedPath(path: string): boolean {
  // A leading "//" is a network-path reference. If it reaches new URL(path,
  // origin), the first segment becomes an attacker-controlled hostname rather
  // than an application path.
  if (!path.startsWith("/") || path.startsWith("//") || path.includes("#") || path.includes("\\") || /[\u0000-\u001F\u007F]/.test(path)) {
    return false;
  }
  const origin = "https://local.invalid";
  let url: URL;
  try {
    url = new URL(path, origin);
  } catch {
    return false;
  }
  if (url.origin !== origin) return false;
  const segments = splitPath(url.pathname);
  const embed = parseEmbedSegments(segments);
  const query = Array.from(url.searchParams);
  const imgIndex = query.length === 1 && query[0][0] === "img_index"
    ? parseCanonicalDecimal(query[0][1])
    : null;
  if (embed) {
    return query.length === 0
      || (imgIndex !== null && imgIndex > 0 && imgIndex <= maxPostMediaItems);
  }
  return query.length === 0 && (
    parseStoriesSegments(segments) !== null
    || (segments.length === 1 && validUsername(segments[0]))
  );
}

export function instagramEmbedPath(raw: string): string | null {
  let value = raw.trim();
  if (!value) return null;
  if (!/^https?:\/\//i.test(value)) value = `https://${value}`;
  let url: URL;
  try {
    url = new URL(value);
  } catch {
    return null;
  }
  const hostname = url.hostname.toLowerCase();
  if (hostname !== "instagram.com" && !hostname.endsWith(".instagram.com")) return null;
  const segments = splitPath(url.pathname);
  const path = `/${segments.join("/")}`;
  const embed = parseEmbedSegments(segments);
  const selected = embed?.pathIndex === null ? mediaSelection(url.searchParams, null) : null;
  const canonical = selected === null ? path : `${path}?img_index=${selected + 1}`;
  return validEmbedPath(canonical) ? canonical : null;
}

export function mastodonStatusPathFromAlternate(href: string, origin: string): string | null {
  try {
    const url = new URL(href, origin);
    const segments = splitPath(url.pathname);
    if (
      url.origin !== new URL(origin).origin
      || segments.length !== 4
      || segments[0] !== "users"
      || !validUsername(segments[1])
      || segments[2] !== "statuses"
      || !/^\d{1,256}$/.test(segments[3])
    ) {
      return null;
    }
    return `/api/v1/statuses/${segments[3]}`;
  } catch {
    return null;
  }
}

const instagramOrigin = "https://www.instagram.com";

export type ContainerRoute = {
  cacheKey: string;
  humanRedirect?: string;
  rewritePath?: string;
  allowOffloadFetch?: boolean;
  offloadPath?: string;
  metric?: "embed" | "direct" | "profile" | "story";
};

function isDirectHost(url: URL): boolean {
  return url.hostname === "d.oginstagram.com" || url.hostname === "www.d.oginstagram.com";
}

function isGalleryHost(url: URL): boolean {
  return url.hostname === "g.oginstagram.com" || url.hostname === "www.g.oginstagram.com";
}

function internalRewritePath(url: URL, gallery: boolean): string | undefined {
  const rewritten = new URL(url.pathname + url.search, url.origin);
  // __gallery is an internal transport marker, never public input. Removing it
  // first prevents a plain-host request from priming a gallery response under
  // the normal embed cache key.
  rewritten.searchParams.delete("__gallery");
  if (gallery) rewritten.searchParams.set("__gallery", "1");
  const path = rewritten.pathname + rewritten.search;
  return path === url.pathname + url.search ? undefined : path;
}

function validPositiveOffloadIndex(value: string, allowMp4: boolean, maximum: number): boolean {
  const decimal = allowMp4 && value.endsWith(".mp4") ? value.slice(0, -4) : value;
  const parsed = parseCanonicalDecimal(decimal);
  return parsed !== null && parsed > 0 && parsed <= maximum;
}

function validOffloadPath(path: string, segments: string[]): boolean {
  if (segments[0] !== "offload" || path !== `/${segments.join("/")}`) {
    return false;
  }

  if (segments[1] === "story" && (segments.length === 4 || segments.length === 5)) {
    return validUsername(segments[2])
      && storyIdPattern.test(segments[3])
      && (segments.length === 4 || segments[4] === "avatar");
  }

  if (segments.length !== 2 && segments.length !== 3) {
    return false;
  }

  const subject = segments[1];
  const suffix = segments[2];
  if (subject.startsWith("@")) {
    if (!validUsername(subject.slice(1))) {
      return false;
    }
    return suffix === undefined || suffix === "avatar" || validPositiveOffloadIndex(suffix, false, maxProfileMediaItems);
  }

  if (!validShortcode(subject)) {
    return false;
  }
  return suffix === undefined || suffix === "avatar" || validPositiveOffloadIndex(suffix, true, maxPostMediaItems);
}

function canonicalOffloadPath(segments: string[]): string {
  const canonical = [...segments];
  if (canonical[1] === "story" && canonical.length >= 4) {
    canonical[2] = canonical[2].toLowerCase();
  } else if (canonical[1]?.startsWith("@")) {
    canonical[1] = `@${canonical[1].slice(1).toLowerCase()}`;
  }
  return `/${canonical.join("/")}`;
}

export function resolveContainerRoute(url: URL): ContainerRoute | null {
  const path = url.pathname;
  const segments = splitPath(path);
  if (path === "/.well-known/webfinger") {
    return { cacheKey: path + url.search };
  }
  if (validOffloadPath(path, segments)) {
    const offloadPath = canonicalOffloadPath(segments);
    const thumbnail = url.searchParams.has("thumbnail") ? "?thumbnail=1" : "";
    return { cacheKey: `${offloadPath}${thumbnail}`, offloadPath };
  }
  if (segments[0] === "offload") {
    return null;
  }
  const stories = parseStoriesSegments(segments);
  if (stories) {
    const username = encodeURIComponent(stories.username);
    const canonicalUsername = encodeURIComponent(stories.username.toLowerCase());
    if (isDirectHost(url)) {
      return {
        cacheKey: `/direct/story/${canonicalUsername}/${stories.storyId}`,
        rewritePath: `/offload/story/${canonicalUsername}/${stories.storyId}`,
        allowOffloadFetch: true,
        metric: "story"
      };
    }
    const gallery = isGalleryHost(url);
    return {
      cacheKey: `/${gallery ? "gallery" : "embed"}/story/${canonicalUsername}/${stories.storyId}`,
      humanRedirect: `${instagramOrigin}/stories/${username}/${stories.storyId}/`,
      rewritePath: internalRewritePath(url, gallery),
      metric: "story"
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
      humanRedirect: origin,
      rewritePath: internalRewritePath(url, gallery),
      metric: "embed"
    };
  }

  if (segments.length === 1 && validUsername(segments[0])) {
    const username = segments[0];
    const gallery = isGalleryHost(url);
    return {
      cacheKey: `/${gallery ? "pgallery" : "profile"}/${encodeURIComponent(username.toLowerCase())}`,
      humanRedirect: `${instagramOrigin}/${encodeURIComponent(username)}/`,
      rewritePath: internalRewritePath(url, gallery),
      metric: "profile"
    };
  }
  return null;
}

// Strips the service subdomain prefixes for display. Unlike edgeCacheKey (which
// keeps d./g. distinct for cache isolation), this collapses every variant to the
// bare host shown to users.
export function canonicalServiceHost(host: string): string {
  return host.replace(/^(?:(?:www|g|d)\.)+/i, "");
}

// Workers Caching excludes the hostname from its default key. The response body
// contains host-bound URLs, so every public hostname needs its own cache entry.
export function edgeCacheKey(route: ContainerRoute, url: URL): string {
  return `${url.origin}${route.cacheKey}`;
}
