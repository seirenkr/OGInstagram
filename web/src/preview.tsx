import React, { useEffect, useMemo, useRef, useState } from "react";
import { Button } from "@cloudflare/kumo/components/button";
import { Input } from "@cloudflare/kumo/components/input";
import { PaperPlaneRightIcon as PaperPlaneRight } from "@phosphor-icons/react/PaperPlaneRight";
import { ArrowSquareOutIcon as ArrowSquareOut } from "@phosphor-icons/react/ArrowSquareOut";
import { PlayIcon as Play } from "@phosphor-icons/react/Play";
import { SealCheckIcon as SealCheck } from "@phosphor-icons/react/SealCheck";
import { parse as parseEmoji } from "@twemoji/parser";
import { version as twemojiVersion } from "@twemoji/svg/package.json";
import { canonicalServiceHost, instagramEmbedPath } from "../../shared/routes.ts";
import type en from "../locales/en.json";
import { requestPreview } from "./preview-request.ts";
import {
  formatTimestamp, galleryRows, httpURL, offloadPoster, parseComponentEmbed,
  type ComponentEmbed, type EmbedChild, type LinkButton, type Markdown, type Media,
} from "./component-embed.ts";

declare global {
  interface Window {
    turnstile?: {
      render: (element: HTMLElement, options: {
        sitekey: string;
        action: string;
        appearance: "execute";
        execution: "execute";
        callback: (token: string) => void;
        "error-callback": () => void;
        "expired-callback": () => void;
        "timeout-callback": () => void;
        "response-field": false;
      }) => string;
      execute: (widgetId: string) => void;
      reset: (widgetId: string) => void;
      remove: (widgetId: string) => void;
    };
    onloadTurnstileCallback?: () => void;
  }
}

type PreviewPost = {
  id: number;
  author: string;
  avatar: string;
  time: string;
  path: string;
  title?: string;
  profileUrl?: string;
  authorIconUrl?: string;
  statsText?: string;
  caption?: string;
  media?: Media;
  mediaIsVideo?: boolean;
  date?: string;
  component?: ComponentEmbed;
};

type PreviewProps = {
  brand: string;
  host: string;
  lang: string;
  copy: typeof en.hero;
  turnstileSiteKey: string;
  reduceMotion: boolean;
};

const TURNSTILE_SCRIPT = "https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit&onload=onloadTurnstileCallback";
const STATS_EMOJI_RE = /❤️|💬|📝|👤|▶️/;
const TWEMOJI_BASE = `https://cdn.jsdelivr.net/npm/@twemoji/svg@${twemojiVersion}/`;
const COMPONENT_EMBED_TYPES = new Set(["application/json", "application/vnd.discord.component-embed+json"]);
let turnstileReady: Promise<void> | null = null;

function loadTurnstile(): Promise<void> {
  if (window.turnstile) return Promise.resolve();
  if (turnstileReady) return turnstileReady;
  turnstileReady = new Promise((resolve, reject) => {
    const script = document.createElement("script");
    const fail = () => {
      script.remove();
      window.onloadTurnstileCallback = undefined;
      turnstileReady = null;
      reject(new Error("Turnstile failed to load"));
    };
    window.onloadTurnstileCallback = () => {
      window.onloadTurnstileCallback = undefined;
      if (window.turnstile) resolve();
      else fail();
    };
    script.src = TURNSTILE_SCRIPT;
    script.async = true;
    script.onerror = fail;
    document.head.append(script);
  });
  return turnstileReady;
}

// Callers pass an already-canonicalized service host (see app.tsx).
export function HighlightedHost({ host }: { host: string }) {
  if (!host.toLowerCase().startsWith("og")) return host;
  return <><strong className="text-kumo-brand">{host.slice(0, 2)}</strong>{host.slice(2)}</>;
}

function TwemojiImage({ text, url }: { text: string; url: string }) {
  const [failed, setFailed] = useState(false);
  if (failed) return text;
  return <img className="inline-block h-[1.1em] w-[1.1em] align-[-0.18em]" src={url}
    alt={text} draggable={false} width={20} height={20} loading="lazy" decoding="async"
    onError={() => setFailed(true)} />;
}

const TwemojiText = React.memo(function TwemojiText({ children }: { children: string }) {
  const emojis = parseEmoji(children, { buildUrl: (codepoints) => `${TWEMOJI_BASE}${codepoints}.svg` });
  if (!emojis.length) return children;
  const parts: React.ReactNode[] = [];
  let cursor = 0;
  for (const emoji of emojis) {
    const [start, end] = emoji.indices;
    if (start > cursor) parts.push(children.slice(cursor, start));
    // A lone U+FE0F parses with an empty url; keep it as text.
    parts.push(emoji.url ? <TwemojiImage key={`${start}-${emoji.text}`} text={emoji.text} url={emoji.url} /> : emoji.text);
    cursor = end;
  }
  if (cursor < children.length) parts.push(children.slice(cursor));
  return <>{parts}</>;
});

function previewMediaUrl(raw: string, serviceHost: string): string | undefined {
  if (!raw) return undefined;
  let url: URL;
  try { url = new URL(raw, location.origin); } catch { return undefined; }
  if (url.protocol !== "http:" && url.protocol !== "https:") return undefined;
  const localHost = serviceHost.toLowerCase().split(":")[0];
  const urlHost = canonicalServiceHost(url.hostname.toLowerCase());
  if (localHost && urlHost === localHost) {
    url = new URL(`${url.pathname}${url.search}`, location.origin);
  }
  return url.href;
}

function previewDate(raw: unknown, dateFormatter: Intl.DateTimeFormat): string | undefined {
  if (typeof raw !== "string" || !raw) return undefined;
  const date = new Date(raw);
  return Number.isNaN(date.getTime()) ? undefined : dateFormatter.format(date);
}

function parseHTMLPreview(doc: Document, serviceHost: string, dateFormatter: Intl.DateTimeFormat): Partial<PreviewPost> {
  const meta = (key: string) => doc.querySelector(`meta[property="${key}"], meta[name="${key}"]`)?.getAttribute("content") ?? "";
  const title = meta("og:title");
  if (!title) throw new Error("no embed metadata");
  const script = doc.head.querySelector('script[id="discord:component-embed"]');
  const component = script && COMPONENT_EMBED_TYPES.has(script.getAttribute("type") ?? "")
    ? parseComponentEmbed(script.textContent ?? "", (url) => previewMediaUrl(url, serviceHost)) ?? undefined
    : undefined;
  const description = meta("og:description");
  const [firstBlock = "", ...restBlocks] = description.split("\n\n");
  const hasStats = STATS_EMOJI_RE.test(firstBlock);
  const isProfile = meta("og:type") === "profile";
  const image = previewMediaUrl(meta("og:image"), serviceHost);
  const authorIcon = previewMediaUrl(
    doc.querySelector('link[rel="apple-touch-icon"]')?.getAttribute("href") ?? "",
    serviceHost
  ) || (isProfile && image ? image : "");
  return {
    title,
    component,
    profileUrl: httpURL(meta("article:author")),
    authorIconUrl: authorIcon || undefined,
    statsText: hasStats ? firstBlock : undefined,
    caption: (hasStats ? restBlocks.join("\n\n") : description) || undefined,
    media: image && !image.includes("/favicon-") ? { url: image, alt: "" } : undefined,
    mediaIsVideo: Boolean(meta("og:video")),
    date: previewDate(meta("article:published_time"), dateFormatter),
  };
}

async function fetchPreview(path: string, token: string | undefined, signal: AbortSignal, serviceHost: string, dateFormatter: Intl.DateTimeFormat): Promise<Partial<PreviewPost>> {
  const response = await requestPreview(path, token, signal);
  if (response.status === 429) throw new Error("rate-limited");
  if (response.status === 404 || response.status === 451) throw new Error("unavailable");
  if (!response.ok) throw new Error(`status ${response.status}`);
  const contentType = response.headers.get("content-type")?.toLowerCase() ?? "";
  if (!contentType.includes("text/html")) throw new Error("invalid preview response");
  const doc = new DOMParser().parseFromString(await response.text(), "text/html");
  return parseHTMLPreview(doc, serviceHost, dateFormatter);
}

const EMOJI_CLASS = "inline-block h-[1.1em] w-[1.1em] align-[-0.18em]";

// Discord's emoji CDN is outside the page's img-src, so only our own two render.
function CustomEmoji({ name }: { name: string }) {
  switch (name.toLowerCase()) {
    case "verified":
      // docs/discord-emojis/verified.svg: this icon over a white disc.
      return <span className={`relative ${EMOJI_CLASS}`} role="img" aria-label={`:${name}:`}>
        <span className="absolute inset-1/4 rounded-full bg-white" />
        <SealCheck className="relative h-full w-full" weight="fill" color="#0095f6" aria-hidden="true" />
      </span>;
    case "oginstagram":
      return <img className={EMOJI_CLASS} src="/favicon-64.png" alt={`:${name}:`} width={20} height={20} />;
    default:
      return null;
  }
}

const MARKDOWN_ELEMENTS = {
  b: ["strong", undefined],
  i: ["em", undefined],
  u: ["u", undefined],
  s: ["s", undefined],
  h1: ["span", "my-0.5 block text-[1.2rem] font-bold leading-tight"],
  h2: ["span", "my-0.5 block text-[1rem] font-bold leading-tight"],
  h3: ["span", "my-0.5 block font-bold leading-tight"],
  sub: ["span", "block text-[0.7rem] leading-snug text-kumo-subtle"],
} as const;

function MarkdownNodes({ nodes, lang }: { nodes: Markdown[]; lang: string }) {
  return nodes.map((node, key) => {
    if (typeof node === "string") return <TwemojiText key={key}>{node}</TwemojiText>;
    switch (node.tag) {
      case "emoji":
        return <CustomEmoji key={key} name={node.name} />;
      case "time":
        return <time key={key} className="rounded bg-kumo-recessed px-0.5" dateTime={new Date(node.unix * 1000).toISOString()}>{formatTimestamp(node.unix, node.style, lang)}</time>;
      case "a":
        return <a key={key} className="text-kumo-link no-underline hover:underline" href={node.href} target="_blank" rel="noopener noreferrer"><MarkdownNodes nodes={node.children} lang={lang} /></a>;
      default: {
        const [Tag, className] = MARKDOWN_ELEMENTS[node.tag];
        return <Tag key={key} className={className}><MarkdownNodes nodes={node.children} lang={lang} /></Tag>;
      }
    }
  });
}

function TextDisplay({ nodes, lang }: { nodes: Markdown[]; lang: string }) {
  return <div className="whitespace-pre-wrap [overflow-wrap:anywhere]"><MarkdownNodes nodes={nodes} lang={lang} /></div>;
}

function LinkButtonView({ button }: { button: LinkButton }) {
  const className = "inline-flex max-w-full items-center gap-1.5 rounded-lg bg-kumo-fill px-3 py-1.5 text-[0.8rem] font-medium leading-tight text-kumo-default no-underline";
  const content = <>
    <span className="truncate"><TwemojiText>{button.label}</TwemojiText></span>
    <ArrowSquareOut className="flex-none" size={14} aria-hidden="true" />
  </>;
  return button.href
    ? <a className={`${className} hover:bg-kumo-fill-hover`} href={button.href} target="_blank" rel="noopener noreferrer">{content}</a>
    : <span className={`${className} opacity-50`} aria-disabled="true">{content}</span>;
}

type TileProps = { item: Media; videoUnavailable: string; video?: boolean; single?: boolean; className?: string };

function GalleryTile({ item, videoUnavailable, video = false, single = false, className = "" }: TileProps) {
  const [failed, setFailed] = useState(false);
  if (failed) return <div className={`min-h-16 bg-kumo-recessed ${className}`} />;
  return <div className={`relative min-h-0 min-w-0 overflow-hidden bg-kumo-recessed ${className}`}>
    <img className={`block saturate-[0.72] contrast-[1.02] ${single ? "h-auto max-h-80 w-auto max-w-full" : "h-full w-full object-cover"}`}
      src={offloadPoster(item.url, location.origin) ?? item.url} alt={item.alt} loading="lazy" decoding="async"
      onError={() => setFailed(true)} />
    {video ? <span className="absolute inset-0 flex flex-col items-center justify-center gap-1 bg-black/25 px-2 text-center text-white [text-shadow:0_1px_2px_rgb(0_0_0/0.5)]">
      <Play size={26} weight="fill" aria-hidden="true" />
      <span className={single ? "text-[0.65rem] leading-tight" : "sr-only"}>{videoUnavailable}</span>
    </span> : null}
  </div>;
}

const GALLERY_ROW_CLASS = ["", "aspect-[2/1] grid-cols-1", "aspect-[2/1] grid-cols-2", "aspect-[3/1] grid-cols-3"];

function MediaGallery({ items, videoUnavailable }: { items: Media[]; videoUnavailable: string }) {
  if (items.length === 1) {
    return <div className="w-fit max-w-full overflow-hidden rounded-lg"><GalleryTile item={items[0]} videoUnavailable={videoUnavailable} single /></div>;
  }
  if (items.length === 3) {
    return <div className="grid aspect-[3/2] grid-cols-[2fr_1fr] grid-rows-2 gap-1 overflow-hidden rounded-lg">
      {items.map((item, index) => <GalleryTile key={`${item.url}-${index}`} item={item} videoUnavailable={videoUnavailable} className={index === 0 ? "row-span-2" : ""} />)}
    </div>;
  }
  return <div className="flex flex-col gap-1 overflow-hidden rounded-lg">
    {galleryRows(items).map((row, rowIndex) => <div key={rowIndex} className={`grid gap-1 ${GALLERY_ROW_CLASS[row.length]}`}>
      {row.map((item, index) => <GalleryTile key={`${item.url}-${index}`} item={item} videoUnavailable={videoUnavailable} />)}
    </div>)}
  </div>;
}

function EmbedChildView({ child, lang, videoUnavailable }: { child: EmbedChild; lang: string; videoUnavailable: string }) {
  switch (child.type) {
    case "text":
      return <TextDisplay nodes={child.content} lang={lang} />;
    case "section":
      return <div className={`flex gap-3 ${child.thumbnail ? "items-start" : "items-center"}`}>
        <div className="flex min-w-0 flex-1 flex-col gap-2">{child.texts.map((nodes, index) => <TextDisplay key={index} nodes={nodes} lang={lang} />)}</div>
        {child.thumbnail ? <img className="h-[4.5rem] w-[4.5rem] flex-none rounded-lg object-cover max-sm:h-16 max-sm:w-16"
          src={child.thumbnail.url} alt={child.thumbnail.alt} width={72} height={72} /> : null}
        {child.button ? <div className="flex-none"><LinkButtonView button={child.button} /></div> : null}
      </div>;
    case "gallery":
      return <MediaGallery items={child.items} videoUnavailable={videoUnavailable} />;
    case "separator":
      return <hr className={`${child.large ? "my-2" : "my-0.5"} ${child.divider ? "border-kumo-hairline" : "border-transparent"}`} />;
    case "row":
      return <div className="flex flex-wrap gap-2">{child.buttons.map((button, index) => <LinkButtonView key={index} button={button} />)}</div>;
  }
}

function ComponentEmbedView({ embed, lang, videoUnavailable }: { embed: ComponentEmbed; lang: string; videoUnavailable: string }) {
  return <article className="hero-item mt-3 flex max-w-[30rem] flex-col gap-2 rounded-lg border border-l-4 border-kumo-hairline bg-kumo-base p-3 text-[0.8rem] leading-[1.45] text-kumo-default max-sm:text-xs"
    style={embed.accent ? { borderLeftColor: embed.accent } : undefined}>
    {embed.children.map((child, index) => <EmbedChildView key={index} child={child} lang={lang} videoUnavailable={videoUnavailable} />)}
  </article>;
}

// The demo post, in the layout the bot sends to Discord.
function sampleEmbed(): ComponentEmbed | undefined {
  const asset = (file: string) => ({ url: `${location.origin}/preview/v1/${file}` });
  return parseComponentEmbed(JSON.stringify({ component: { type: 17, accent_color: 0xff0069, components: [
    { type: 9, components: [{ type: 10, content: "**Visit Türkiye**\n@visit-turkiye\n\n❤️ **12.4K**  💬 **84**" }], accessory: { type: 11, media: asset("avatar-2.svg") } },
    { type: 10, content: "Antalya, where the Taurus Mountains meet the Mediterranean.\nOld town lanes, turquoise coves, and the Red Tower glowing at sunset.\n\n#Antalya #Türkiye #TravelGuide" },
    { type: 12, items: [{ media: asset("antalya.webp") }] },
    { type: 9, components: [{ type: 10, content: `-# <:OGInstagram:1556597080229810266> OGInstagram · <t:${Math.floor(Date.now() / 1000)}:s>` }],
      accessory: { type: 2, style: 5, label: "📷 Instagram", url: "https://www.instagram.com/" } },
  ] } })) ?? undefined;
}

function Preview({ brand, host, lang, copy, turnstileSiteKey, reduceMotion }: PreviewProps) {
  const baseHost = host.toLowerCase().startsWith("og") ? host.slice(2) : host;
  const timeFormatter = useMemo(() => new Intl.DateTimeFormat(lang, { hour: "numeric", minute: "numeric" }), [lang]);
  const dateFormatter = useMemo(() => new Intl.DateTimeFormat(lang, { year: "numeric", month: "numeric", day: "numeric" }), [lang]);
  const samplePost = useMemo<PreviewPost>(() => ({
    id: 0,
    author: "Phibi",
    avatar: "/preview/v1/avatar-1.svg",
    time: timeFormatter.format(new Date()),
    path: "/p/ExAmpl3",
    component: sampleEmbed(),
  }), [timeFormatter]);
  const [post, setPost] = useState<PreviewPost | null>(null);
  const postId = post?.id;
  const [stage, setStage] = useState<"sent" | "morph" | "embed">("sent");
  const [previewActive, setPreviewActive] = useState(true);
  const [inputValue, setInputValue] = useState("");
  const [typewriterState, setTypewriterState] = useState<"typing" | "paused" | "done">("typing");
  const [error, setError] = useState<string | null>(null);
  const [verifying, setVerifying] = useState(false);
  const [loading, setLoading] = useState(false);
  const composer = useRef<HTMLDivElement>(null);
  const input = useRef<HTMLInputElement>(null);
  const typewriterText = useRef<HTMLSpanElement>(null);
  const typewriterCaret = useRef<HTMLSpanElement>(null);
  const lastPostId = useRef(0);
  const previewAbort = useRef<AbortController | null>(null);
  const turnstileHost = useRef<HTMLDivElement>(null);
  const turnstileWidget = useRef<string | null>(null);
  const pendingPath = useRef<string | null>(null);
  const verificationAttempt = useRef(0);
  const wasVerifying = useRef(false);

  useEffect(() => () => {
    verificationAttempt.current++;
    pendingPath.current = null;
    previewAbort.current?.abort();
    if (turnstileWidget.current !== null) {
      window.turnstile?.remove(turnstileWidget.current);
      turnstileWidget.current = null;
    }
  }, []);

  function cancelPreviewWork() {
    verificationAttempt.current++;
    const hadPendingVerification = pendingPath.current !== null;
    pendingPath.current = null;
    if (hadPendingVerification && turnstileWidget.current !== null) {
      window.turnstile?.reset(turnstileWidget.current);
    }
    if (hadPendingVerification) setVerifying(false);
    previewAbort.current?.abort();
    previewAbort.current = null;
    setLoading(false);
  }

  function failVerification() {
    if (pendingPath.current === null) return;
    cancelPreviewWork();
    setError(copy.fetchError);
  }

  function executeTurnstile() {
    const turnstile = window.turnstile;
    const hostElement = turnstileHost.current;
    if (!turnstile || !hostElement) throw new Error("Turnstile unavailable");
    if (turnstileWidget.current === null) {
      turnstileWidget.current = turnstile.render(hostElement, {
        sitekey: turnstileSiteKey,
        action: "turnstile-spin-v1",
        appearance: "execute",
        execution: "execute",
        "response-field": false,
        callback: (token) => {
          const path = pendingPath.current;
          if (!path) return;
          void runPreview(path, token).finally(() => {
            pendingPath.current = null;
            if (turnstileWidget.current !== null) window.turnstile?.reset(turnstileWidget.current);
            setVerifying(false);
          });
        },
        "error-callback": failVerification,
        "expired-callback": failVerification,
        "timeout-callback": failVerification,
      });
    }
    turnstile.execute(turnstileWidget.current);
  }

  useEffect(() => {
    if (!previewActive || reduceMotion) return;
    const source = `https://${baseHost}${samplePost.path}`;
    const text = typewriterText.current;
    const caret = typewriterCaret.current;
    if (!text || !caret) return;
    const duration = source.length * 30;
    const easing = `steps(${source.length}, end)`;
    const width = Math.max(0, text.getBoundingClientRect().width - 2);
    const textAnimation = text.animate(
      [{ clipPath: "inset(0 100% 0 0)" }, { clipPath: "inset(0 0% 0 0)" }],
      { duration, easing, fill: "forwards" }
    );
    const caretAnimation = caret.animate(
      [{ transform: "translateX(0)" }, { transform: `translateX(${width}px)` }],
      { duration, easing, fill: "forwards" }
    );
    const pauseTimer = window.setTimeout(() => {
      setTypewriterState("paused");
      postTimer = window.setTimeout(() => {
        setTypewriterState("done");
        setPost(samplePost);
      }, 800);
    }, duration);
    let postTimer: number | undefined;
    return () => {
      textAnimation.cancel();
      caretAnimation.cancel();
      window.clearTimeout(pauseTimer);
      window.clearTimeout(postTimer);
    };
  }, [baseHost, previewActive, reduceMotion, samplePost]);

  // The disabled input dropped focus to <body> while verifying.
  useEffect(() => {
    if (wasVerifying.current && !verifying && document.activeElement === document.body) input.current?.focus();
    wasVerifying.current = verifying;
  }, [verifying]);

  useEffect(() => {
    if (postId === undefined || reduceMotion) return;
    const morphTimer = window.setTimeout(() => setStage("morph"), 900);
    const embedTimer = window.setTimeout(() => setStage("embed"), 1650);
    return () => {
      window.clearTimeout(morphTimer);
      window.clearTimeout(embedTimer);
    };
  }, [postId, reduceMotion]);

  async function runPreview(path: string, token?: string): Promise<void> {
    previewAbort.current?.abort();
    const controller = new AbortController();
    previewAbort.current = controller;
    setLoading(true);
    setError(null);
    // A token retry fills in the message already on screen.
    const id = token === undefined ? ++lastPostId.current : lastPostId.current;
    if (token === undefined) {
      setInputValue("");
      setStage("sent");
      setPost({ id, author: copy.you, avatar: "/preview/v1/avatar-3.svg", time: timeFormatter.format(new Date()), path });
    }
    try {
      const live = await fetchPreview(path, token, controller.signal, host, dateFormatter);
      if (!controller.signal.aborted) setPost((current) => current && current.id === id ? { ...current, ...live } : current);
    } catch (reason: unknown) {
      if (controller.signal.aborted) return;
      if (token === undefined && reason instanceof Error && reason.message === "cloudflare-challenge") {
        const attempt = ++verificationAttempt.current;
        pendingPath.current = path;
        setVerifying(true);
        const current = () => verificationAttempt.current === attempt && pendingPath.current === path;
        const timeout = new Promise<never>((_, reject) => window.setTimeout(() => reject(new Error("Turnstile load timed out")), 15_000));
        void Promise.race([loadTurnstile(), timeout])
          .then(() => { if (current()) executeTurnstile(); })
          .catch(() => { if (current()) failVerification(); });
        return;
      }
      const message = reason instanceof Error ? reason.message : "";
      setError(message === "rate-limited" ? copy.rateLimited : message === "unavailable" ? copy.unavailable : copy.fetchError);
    } finally {
      if (previewAbort.current === controller) {
        previewAbort.current = null;
        setLoading(false);
      }
    }
  }

  function submitLink(event: React.FormEvent) {
    event.preventDefault();
    if (pendingPath.current) return;
    cancelPreviewWork();
    setPreviewActive(false);
    const path = instagramEmbedPath(inputValue);
    if (!path) {
      setError(copy.invalid);
      if (!reduceMotion) {
        composer.current?.animate(
          { transform: ["translateX(0)", "translateX(-7px)", "translateX(7px)", "translateX(-5px)", "translateX(5px)", "translateX(0)"] },
          { duration: 360, easing: "ease-out" }
        );
      }
      return;
    }
    void runPreview(path);
  }

  const visiblePost = post ?? (previewActive && reduceMotion ? samplePost : null);
  const visibleStage = reduceMotion ? "embed" : stage;
  return <figure className="m-0 w-full">
    <figcaption className="sr-only">{copy.previewDesc}</figcaption>
    <div className="flex h-[32rem] flex-col overflow-hidden rounded-[14px] border border-kumo-hairline bg-kumo-elevated text-kumo-default shadow-[0_30px_80px_-48px_var(--color-kumo-shadow-drop)] [contain:layout_paint]">
      <div className="preview-titlebar flex h-[2.4rem] flex-none items-center gap-3 border-b border-[color-mix(in_srgb,var(--color-kumo-hairline)_86%,transparent)] bg-[linear-gradient(180deg,color-mix(in_srgb,var(--color-kumo-base)_62%,var(--color-kumo-elevated)),color-mix(in_srgb,var(--color-kumo-recessed)_72%,var(--color-kumo-elevated)))] px-3.5 max-sm:h-[2.125rem] max-sm:px-3"
        aria-hidden="true">
        <div className="flex gap-[0.44rem]">
          {["bg-[#ff5f57]", "bg-[#febc2e]", "bg-[#28c840]"].map((color) => <span key={color}
            className={`preview-window-dot h-2.5 w-2.5 rounded-full border border-black/15 shadow-[inset_0_1px_0_rgb(255_255_255/0.28),0_0.5px_0_rgb(0_0_0/0.08)] ${color}`} />)}
        </div>
        <span className="text-xs font-semibold text-kumo-subtle" translate="no">{copy.channel}</span>
      </div>
      <div className="relative min-h-0 flex-1 overflow-y-auto overscroll-contain p-4 [scrollbar-width:thin]" aria-live={previewActive ? "off" : "polite"}>
          {visiblePost ? <div key={visiblePost.id} className="hero-item grid grid-cols-[2.25rem_minmax(0,1fr)] gap-3 max-sm:grid-cols-[2rem_minmax(0,1fr)] max-sm:gap-2.5">
            <img className="h-9 w-9 rounded-full border border-[color-mix(in_srgb,var(--color-kumo-brand)_20%,var(--color-kumo-hairline))] bg-[color-mix(in_srgb,var(--color-kumo-brand)_9%,var(--color-kumo-base))] object-cover max-sm:h-8 max-sm:w-8"
              src={visiblePost.avatar} alt="" width={36} height={36} aria-hidden="true" />
            <div className="min-w-0">
              <div className="flex items-baseline gap-2 leading-tight"><strong className="text-sm font-semibold text-kumo-default" translate="no">{visiblePost.author}</strong><span className="text-[0.67rem] text-kumo-subtle">{visiblePost.time}</span></div>
              <div className="relative mt-1.5 h-[1.4rem] overflow-hidden text-[0.8rem] leading-[1.4rem] max-sm:text-xs" translate="no">
                <div className={`preview-url-source absolute inset-0 truncate text-kumo-subtle ${visibleStage === "sent" ? "" : "preview-url-hidden"}`}>https://{baseHost}{visiblePost.path}</div>
                <div className={`preview-url-target absolute inset-0 truncate text-kumo-default ${visibleStage === "sent" ? "" : "preview-url-visible"}`}><span>https://</span><HighlightedHost host={host} /><span>{visiblePost.path}</span></div>
              </div>
              {visibleStage === "embed" && visiblePost.component
                ? <ComponentEmbedView embed={visiblePost.component} lang={lang} videoUnavailable={copy.videoUnavailable} />
                : visibleStage === "embed" && visiblePost.title ? <article className="hero-item mt-3 rounded border-l-4 border-kumo-brand bg-kumo-base py-2 pr-4 pb-4 pl-3 [overflow-wrap:anywhere] max-sm:pr-3">
                <div className="mt-2 flex items-center gap-2">
                  <img className="h-6 w-6 rounded-full object-cover" src={visiblePost.authorIconUrl ?? visiblePost.avatar} alt="" width={24} height={24} />
                  {visiblePost.profileUrl ? <a className="text-sm font-semibold text-kumo-default no-underline hover:underline" href={visiblePost.profileUrl} target="_blank" rel="noreferrer noopener" translate="no">{visiblePost.title}</a> : <span className="text-sm font-semibold text-kumo-default" translate="no">{visiblePost.title}</span>}
                </div>
                {visiblePost.statsText || visiblePost.caption ? <div className="mt-2 flex flex-col gap-2 text-[0.8rem] leading-[1.45] text-kumo-default max-sm:text-xs">
                  {visiblePost.statsText ? <p><strong translate="no"><TwemojiText>{visiblePost.statsText}</TwemojiText></strong></p> : null}
                  {visiblePost.caption ? <p className="whitespace-pre-line"><TwemojiText>{visiblePost.caption}</TwemojiText></p> : null}
                </div> : null}
                {visiblePost.media ? <div className="mt-2.5 w-fit max-w-[225px] overflow-hidden rounded">
                  <GalleryTile item={visiblePost.media} videoUnavailable={copy.videoUnavailable} video={visiblePost.mediaIsVideo} single />
                </div> : null}
                <div className="mt-2.5 flex items-center gap-2"><img className="h-5 w-5 rounded-full object-cover" src="/favicon-64.png" alt="" width={20} height={20} /><span className="text-[0.7rem] leading-tight text-kumo-subtle" translate="no">{brand}{visiblePost.date ? <><i className="mx-1 not-italic">•</i>{visiblePost.date}</> : null}</span></div>
              </article> : null}
            </div>
          </div> : null}
      </div>
      <form className="flex flex-none flex-wrap items-start gap-2 border-t border-[color-mix(in_srgb,var(--color-kumo-hairline)_86%,transparent)] bg-[color-mix(in_srgb,var(--color-kumo-recessed)_55%,var(--color-kumo-elevated))] p-3 pb-3.5" aria-busy={loading || verifying} onSubmit={submitLink}>
        <div ref={composer} className="composer-pulse relative min-w-0 flex-1 rounded-lg [&_label]:hidden">
          <Input ref={input} className={`w-full ${previewActive && !reduceMotion && typewriterState !== "done" ? "text-transparent caret-transparent placeholder:text-transparent" : ""}`}
            value={inputValue} onChange={(event) => {
              setInputValue(event.target.value);
              setPreviewActive(false);
              if (error) setError(null);
            }}
            onFocus={() => {
              if (turnstileSiteKey) void loadTurnstile().catch(() => {});
              if (previewActive) {
                cancelPreviewWork();
                setPreviewActive(false);
                setInputValue("");
              }
            }} placeholder={previewActive && !reduceMotion && typewriterState !== "done" ? "" : copy.placeholder}
            label={copy.placeholder} aria-label={copy.placeholder} error={error ?? undefined} disabled={verifying} translate="no" passwordManagerIgnore autoComplete="off" spellCheck={false} inputMode="url" enterKeyHint="go" />
          {previewActive && !reduceMotion && typewriterState !== "done" ? <span aria-hidden="true" className="pointer-events-none absolute inset-y-0 left-3 right-3 flex items-center overflow-hidden whitespace-pre text-base text-kumo-default">
            <span className="relative inline-block max-w-full">
              <span ref={typewriterText} className="typewriter-text block truncate">{`https://${baseHost}${samplePost.path}`}</span>
              <span ref={typewriterCaret} className={`absolute top-[0.1em] left-[0.2em] inline-block h-[1em] w-[2px] bg-kumo-brand ${typewriterState === "paused" ? "typewriter-caret-blink" : ""}`} />
            </span>
          </span> : null}
          <p role="alert" className="sr-only">{error}</p>
        </div>
        <Button type="submit" variant="primary" shape="square" icon={PaperPlaneRight} aria-label={copy.submit} disabled={!turnstileSiteKey || verifying} />
        <div ref={turnstileHost} className="basis-full empty:hidden" />
      </form>
    </div>
  </figure>;
}

export default React.memo(Preview);
