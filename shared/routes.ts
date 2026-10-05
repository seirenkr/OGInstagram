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

const maxPostMediaItems = 50;
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

// Collapse the public service variants to the host displayed in the UI.
export function canonicalServiceHost(host: string): string {
  return host.replace(/^(?:(?:www|g|d)\.)+/i, "");
}
