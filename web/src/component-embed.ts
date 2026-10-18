// Parses a Discord component embed (https://docs.discord.com/developers/link-previews/component-embeds)
// into plain data for the preview. A payload the docs call invalid returns null,
// so the preview falls back to Open Graph as Discord does.

export type Markdown =
  | string
  | { tag: "b" | "i" | "u" | "s" | "h1" | "h2" | "h3" | "sub"; children: Markdown[] }
  | { tag: "a"; href: string; children: Markdown[] }
  | { tag: "emoji"; name: string }
  | { tag: "time"; unix: number; style: string };

export type LinkButton = { label: string; href?: string };
export type Media = { url: string; alt: string };

export type EmbedChild =
  | { type: "text"; content: Markdown[] }
  | { type: "section"; texts: Markdown[][]; thumbnail?: Media; button?: LinkButton }
  | { type: "gallery"; items: Media[] }
  | { type: "separator"; divider: boolean; large: boolean }
  | { type: "row"; buttons: LinkButton[] };

export type ComponentEmbed = { accent?: string; children: EmbedChild[] };

const MAX_BYTES = 3000;
const MAX_COMPONENTS = 40;
const MAX_GALLERY_ITEMS = 10;
const BUTTON_KEYS = new Set(["id", "type", "url", "style", "label", "emoji", "disabled"]);

type Raw = Record<string, unknown>;
const obj = (v: unknown): Raw | undefined => v && typeof v === "object" && !Array.isArray(v) ? v as Raw : undefined;
const list = (v: unknown, min: number, max: number): unknown[] => {
  if (!Array.isArray(v) || v.length < min || v.length > max) throw new TypeError("invalid component list");
  return v;
};

export function httpURL(raw: unknown): string | undefined {
  if (typeof raw !== "string" || raw.length > 2048) return undefined;
  try {
    const url = new URL(raw);
    return url.protocol === "http:" || url.protocol === "https:" ? url.href : undefined;
  } catch { return undefined; }
}

// media maps a validated absolute URL to the one the page loads.
export function parseComponentEmbed(json: string, media: (url: string) => string | undefined = (url) => url): ComponentEmbed | null {
  if (new TextEncoder().encode(json).length > MAX_BYTES) return null;
  let components = 0, galleryItems = 0;
  const count = (c: Raw | undefined, type: number): Raw => {
    if (c?.type !== type || ++components > MAX_COMPONENTS) throw new TypeError("invalid component");
    return c;
  };
  const mediaOf = (item: Raw): Media => {
    const url = httpURL(obj(item.media)?.url);
    const shown = url && media(url);
    if (!shown) throw new TypeError("invalid media");
    return { url: shown, alt: typeof item.description === "string" ? item.description.slice(0, 1024) : "" };
  };
  const text = (v: unknown): Markdown[] => {
    const c = count(obj(v), 10);
    if (typeof c.content !== "string") throw new TypeError("invalid text");
    return parseMarkdown(c.content);
  };
  const button = (v: unknown): LinkButton => {
    const c = count(obj(v), 2);
    const href = httpURL(c.url);
    const emoji = obj(c.emoji)?.name;
    const label = typeof c.label === "string" ? c.label : typeof emoji === "string" ? emoji : "";
    if (c.style !== 5 || !href || !label || !Object.keys(c).every((key) => BUTTON_KEYS.has(key))) throw new TypeError("invalid button");
    return { label, href: c.disabled === true ? undefined : href };
  };
  try {
    const container = count(obj(obj(JSON.parse(json))?.component), 17);
    const children = list(container.components, 1, MAX_COMPONENTS).map((v): EmbedChild => {
      const c = obj(v);
      switch (c?.type) {
        case 10:
          return { type: "text", content: text(c) };
        case 9: {
          count(c, 9);
          const texts = list(c.components, 1, 3).map(text);
          const accessory = obj(c.accessory);
          if (accessory?.type === 11) return { type: "section", texts, thumbnail: mediaOf(count(accessory, 11)) };
          return { type: "section", texts, button: button(accessory) };
        }
        case 12: {
          count(c, 12);
          const items = list(c.items, 1, MAX_GALLERY_ITEMS).map((item) => mediaOf(obj(item) ?? {}));
          if ((galleryItems += items.length) > MAX_GALLERY_ITEMS) throw new TypeError("too many gallery items");
          return { type: "gallery", items };
        }
        case 14:
          count(c, 14);
          return { type: "separator", divider: c.divider !== false, large: c.spacing === 2 };
        case 1:
          count(c, 1);
          return { type: "row", buttons: list(c.components, 1, 5).map(button) };
        default:
          throw new TypeError("unsupported component");
      }
    });
    const accent = container.accent_color;
    return {
      accent: typeof accent === "number" && Number.isInteger(accent) && accent >= 0 && accent <= 0xffffff
        ? `#${accent.toString(16).padStart(6, "0")}` : undefined,
      children,
    };
  } catch {
    return null;
  }
}

type Mode = "block" | "inline" | "label";

const BLOCK = /^(#{1,3}|-#) +([^\n]+)\n?/;
const ESCAPE = /^\\([^0-9A-Za-z\s])/;
const EMOJI = /^<a?:(\w{2,32}):\d{17,20}>/;
const TIMESTAMP = /^<t:(-?\d{1,13})(?::([tTdDfFsSR]))?>/;
const ANGLE_URL = /^<(https?:\/\/[^\s<>]+)>/;
const BARE_URL = /^(https?:\/\/[^\s<]+[^<.,:;"')\]\s])/;
const MASKED_LINK = /^\[([^[\]]+)\]\(<?(https?:\/\/[^\s()<>]+)>?\)/;
// Discord's simple-markdown inline rules, tried in this order.
const STYLES: [RegExp, "b" | "u" | "s" | "i"][] = [
  [/^\*\*((?:\\[\s\S]|[^\\])+?)\*\*(?!\*)/, "b"],
  [/^__((?:\\[\s\S]|[^\\])+?)__(?!_)/, "u"],
  [/^~~([\s\S]+?)~~/, "s"],
  [/^\*(?=\S)((?:\*\*|\\[\s\S]|\s+(?:\\[\s\S]|[^\s*\\]|\*\*)|[^\s*\\])+?)\*(?!\*)/, "i"],
  [/^_((?:__|\\[\s\S]|[^\\_])+?)_\b/, "i"],
];

function matchAt(rest: string, mode: Mode, lineStart: boolean): [Markdown, number] | undefined {
  let m: RegExpExecArray | null;
  if (mode === "block" && lineStart && (m = BLOCK.exec(rest))) {
    const tag = m[1] === "-#" ? "sub" : `h${m[1].length}` as "h1" | "h2" | "h3";
    return [{ tag, children: parse(m[2], "inline") }, m[0].length];
  }
  // Masked-link labels keep backslashes and cannot nest links.
  if (mode !== "label" && (m = ESCAPE.exec(rest))) return [m[1], m[0].length];
  if ((m = EMOJI.exec(rest))) return [{ tag: "emoji", name: m[1] }, m[0].length];
  if ((m = TIMESTAMP.exec(rest)) && Math.abs(Number(m[1])) <= 8.64e12) {
    return [{ tag: "time", unix: Number(m[1]), style: m[2] ?? "f" }, m[0].length];
  }
  if (mode !== "label") {
    let href: string | undefined;
    if ((m = ANGLE_URL.exec(rest) ?? BARE_URL.exec(rest)) && (href = httpURL(m[1]))) return [{ tag: "a", href, children: [m[1]] }, m[0].length];
    if ((m = MASKED_LINK.exec(rest)) && (href = httpURL(m[2]))) return [{ tag: "a", href, children: parse(m[1], "label") }, m[0].length];
  }
  for (const [re, tag] of STYLES) {
    if ((m = re.exec(rest))) return [{ tag, children: parse(m[1], mode === "label" ? "label" : "inline") }, m[0].length];
  }
}

function parse(src: string, mode: Mode): Markdown[] {
  const out: Markdown[] = [];
  for (let i = 0; i < src.length;) {
    const hit = matchAt(src.slice(i), mode, i === 0 || src[i - 1] === "\n");
    const node = hit ? hit[0] : src[i];
    i += hit ? hit[1] : 1;
    if (typeof node === "string" && typeof out[out.length - 1] === "string") out[out.length - 1] += node;
    else out.push(node);
  }
  return out;
}

export function parseMarkdown(src: string): Markdown[] {
  return parse(src, "block");
}

const TIME_STYLES: Record<string, Intl.DateTimeFormatOptions> = {
  t: { timeStyle: "short" },
  T: { timeStyle: "medium" },
  d: { dateStyle: "short" },
  D: { dateStyle: "long" },
  f: { dateStyle: "long", timeStyle: "short" },
  F: { dateStyle: "full", timeStyle: "short" },
  s: { dateStyle: "short", timeStyle: "short" },
  S: { dateStyle: "short", timeStyle: "medium" },
};

// ponytail: R (relative) renders as f; add Intl.RelativeTimeFormat if a payload ever uses it.
export function formatTimestamp(unix: number, style: string, lang: string): string {
  return new Intl.DateTimeFormat(lang, TIME_STYLES[style] ?? TIME_STYLES.f).format(unix * 1000);
}

// Discord's mosaic: rows of 1-3 tiles, the remainder first. Three items use a
// tall first tile instead, which the renderer lays out itself.
const GALLERY_ROWS = [[1], [2], [1, 2], [2, 2], [2, 3], [3, 3], [1, 3, 3], [2, 3, 3], [3, 3, 3], [1, 3, 3, 3]];

export function galleryRows<T>(items: T[]): T[][] {
  let start = 0;
  return (GALLERY_ROWS[items.length - 1] ?? []).map((size) => items.slice(start, start += size));
}

// Offload URLs redirect to the file itself, and WebKit plays an mp4 in <img>;
// the same signed query with a thumbnail=1 prefix gives its still (a photo's is
// the photo), so the preview never fetches video.
export function offloadPoster(raw: string, origin: string): string | undefined {
  const url = new URL(raw);
  if (url.origin !== origin || !url.pathname.startsWith("/offload/") || url.searchParams.has("thumbnail")) return undefined;
  url.search = `thumbnail=1&${url.search.slice(1)}`;
  return url.href;
}
