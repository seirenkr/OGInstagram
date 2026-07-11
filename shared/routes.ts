export const botRE =
  /bot|discordbot|telegrambot|facebook|twitterbot|slackbot|whatsapp|embed|got|firefox\/92|curl|wget|go-http|yahoo|generator|revoltchat|preview|link|proxy|vkshare|images|analyzer|index|crawl|spider|python|node|deno|mastodon|http\.rb|ruby|bun\/|fiddler|iframely|bluesky|matrix|cardyb|resolver|feedly|rss|reader|atom|thunderbird|axios/i;

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
  const value = Number.parseInt(segments[index], 10);
  return Number.isFinite(value) ? value : undefined;
}

type HomeLocale = "en" | "ja" | "ko" | "zh-hant" | "zh-hans" | "es" | "pt" | "fr";

const HOME_LOCALES: readonly string[] = ["en", "es", "fr", "ja", "ko", "pt", "zh-hans", "zh-hant"];

export function resolveHomeLocale(acceptLanguage: string): HomeLocale {
  for (const part of acceptLanguage.split(",")) {
    const matched = matchLocale(part.split(";")[0].trim().toLowerCase());
    if (matched) {
      return matched;
    }
  }
  return "en";
}

// Chinese needs the script: an explicit Hant script or a TW/HK/MO region picks
// Traditional; any other zh falls back to Simplified.
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
  return parseEmbedSegments(segments) !== null || (segments.length === 1 && validUsername(segments[0]));
}
