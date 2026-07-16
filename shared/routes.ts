export const botRE =
  /bot|facebook|whatsapp|embed|got|firefox\/92|curl|wget|go-http|yahoo|generator|revoltchat|preview|link|proxy|vkshare|images|analyzer|index|crawl|spider|python|node|deno|mastodon|http\.rb|ruby|bun\/|fiddler|iframely|bluesky|matrix|cardyb|resolver|feedly|rss|reader|atom|thunderbird|axios/i;

type EmbedRoute = {
  postType: string;
  shortcode: string;
  pathIndex: number | null;
};

const shortcodeRE = /^[A-Za-z0-9_-]{1,24}$/;

function validShortcode(value: string): boolean {
  return shortcodeRE.test(value);
}

const usernameRE = /^[A-Za-z0-9._]{1,30}$/;

export function validUsername(value: string): boolean {
  return usernameRE.test(value);
}

export function parseStoriesSegments(segments: string[]): { username: string; storyID: string } | null {
  if (segments.length === 3 && segments[0] === "stories" && validUsername(segments[1]) && /^[0-9]{1,32}$/.test(segments[2])) {
    return { username: segments[1], storyID: segments[2] };
  }
  return null;
}

export function parseEmbedSegments(segments: string[]): EmbedRoute | null {
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
  if (pathIndex !== null) return Math.max(0, pathIndex - 1);
  for (const [key, oneBased] of [["img_index", true], ["index", false], ["order", false]] as const) {
    if (!params.has(key)) continue;
    const parsed = parseCanonicalDecimal(params.get(key) ?? "") ?? 0;
    return oneBased ? Math.max(0, parsed - 1) : Math.max(0, parsed);
  }
  return null;
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

export function splitPath(path: string): string[] {
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
  if (!path.startsWith("/") || path.includes("?") || path.includes("#")) {
    return false;
  }
  const segments = splitPath(path);
  return parseEmbedSegments(segments) !== null
    || parseStoriesSegments(segments) !== null
    || (segments.length === 1 && validUsername(segments[0]));
}
