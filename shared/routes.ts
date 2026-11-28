type EmbedRoute = { pathIndex: number | null };

const shortcodePattern = /^[A-Za-z0-9_-]{1,24}$/;

const usernamePattern = /^[A-Za-z0-9._]{1,30}$/;

const storyIdPattern = /^[0-9]{1,32}$/;

const maxPostMediaItems = 50;

const reservedInstagramPaths = new Set([
  "about", "accounts", "api", "challenge",
  "channels", "direct", "download", "explore",
  "graphql", "help", "login", "notifications",
  "p", "popular", "privacy", "profiles",
  "qr", "reels", "session", "signup",
  "stories", "support", "terms", "users",
  "web", "your_activity",
]);

function isStoriesPath(segments: string[]): boolean {
  return segments.length === 3 && segments[0] === "stories" && segments[1] !== "highlights" && usernamePattern.test(segments[1]) && storyIdPattern.test(segments[2]);
}

function parseEmbedSegments(segments: string[]): EmbedRoute | null {
  for (const offset of [0, 1]) {
    const length = segments.length - offset;
    if (
      (length === 2 || length === 3)
      && (offset === 0 || (segments[0] !== "share" && usernamePattern.test(segments[0])))
      && isPostRouteType(segments[offset])
      && shortcodePattern.test(segments[offset + 1])
    ) {
      if (offset === 0 && (segments[0] === "reel" || segments[0] === "reels") && segments[1] === "audio") {
        return null;
      }
      if (length === 2) return { pathIndex: null };
      const pathIndex = parseCanonicalDecimal(segments[offset + 2]);
      return pathIndex === null ? null : { pathIndex };
    }
  }
  return null;
}

function parseShareSegments(segments: string[]): EmbedRoute | null {
  if (segments[0] !== "share" || segments.length < 2) return null;
  const offset = segments[1] === "p" || segments[1] === "reel" ? 2 : 1;
  const length = segments.length - offset;
  if ((length !== 1 && length !== 2) || !shortcodePattern.test(segments[offset])) {
    return null;
  }
  if (length === 1) return { pathIndex: null };
  const pathIndex = parseCanonicalDecimal(segments[offset + 1]);
  return pathIndex === null ? null : { pathIndex };
}

function isPostRouteType(value: string): boolean {
  return value === "p" || value === "reel" || value === "reels";
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
  let decoded: string;
  try {
    decoded = decodeURIComponent(path);
  } catch {
    return [];
  }
  const trimmed = decoded.replace(/^\/+|\/+$/g, "");
  return trimmed ? trimmed.split("/") : [];
}

export function validEmbedPath(path: string): boolean {
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
  const segments = splitPath(path.split("?")[0]);
  if (segments[0] === "offload") return false;
  const embed = parseEmbedSegments(segments) ?? parseShareSegments(segments);
  const query = Array.from(url.searchParams);
  const imgIndex = query.length === 1 && query[0][0] === "img_index"
    ? parseCanonicalDecimal(query[0][1])
    : null;
  if (embed) {
    return query.length === 0
      || (imgIndex !== null && imgIndex > 0 && imgIndex <= maxPostMediaItems);
  }
  return query.length === 0 && (
    isStoriesPath(segments)
    || (segments.length === 1 && usernamePattern.test(segments[0]) && !reservedInstagramPaths.has(segments[0].toLowerCase()))
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
  const embed = parseEmbedSegments(segments) ?? parseShareSegments(segments);
  const selected = embed?.pathIndex === null ? mediaSelection(url.searchParams, null) : null;
  const canonical = selected === null ? path : `${path}?img_index=${selected + 1}`;
  return validEmbedPath(canonical) ? canonical : null;
}

export function canonicalServiceHost(host: string): string {
  return host.replace(/^(?:(?:www|g|d)\.)+/i, "");
}
