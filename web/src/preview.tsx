import React, { useEffect, useMemo, useRef, useState } from "react";
import { Button } from "@cloudflare/kumo/components/button";
import { Input } from "@cloudflare/kumo/components/input";
import { PaperPlaneRightIcon as PaperPlaneRight } from "@phosphor-icons/react/PaperPlaneRight";
import { PlayIcon as Play } from "@phosphor-icons/react/Play";
import { parse as parseEmoji } from "@twemoji/parser";
import { AnimatePresence } from "motion/react";
import * as m from "motion/react-m";
import { canonicalServiceHost, instagramEmbedPath, mastodonStatusPathFromAlternate } from "../../shared/routes.ts";

export type PreviewCopy = {
  line1: string;
  line2: string;
  rich: string;
  channel: string;
  placeholder: string;
  invalid: string;
  fetchError: string;
  rateLimited: string;
  submit: string;
  previewDesc: string;
  you: string;
  videoUnavailable: string;
};

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
  captionNodes?: React.ReactNode[];
  media?: PreviewMedia[];
  date?: string;
};

type PreviewMedia = { url: string; kind: "image" | "video"; width?: number; height?: number };

type PreviewProps = {
  brand: string;
  host: string;
  lang: string;
  copy: PreviewCopy;
  turnstileSiteKey: string;
  reduceMotion: boolean;
};

const TURNSTILE_SCRIPT = "https://challenges.cloudflare.com/turnstile/v0/api.js?render=explicit&onload=onloadTurnstileCallback";
const STATS_EMOJI_RE = /❤️|💬|📝|👤|▶️/;
const TWEMOJI_BASE = "/twemoji/v15.0.0/";
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
  const emojis = useMemo(() => parseEmoji(children, {
    buildUrl: (codepoints) => codepoints ? `${TWEMOJI_BASE}${codepoints}.svg` : "",
  }), [children]);
  if (!emojis.length) return children;
  const parts: React.ReactNode[] = [];
  let cursor = 0;
  for (const emoji of emojis) {
    const [start, end] = emoji.indices;
    if (start > cursor) parts.push(children.slice(cursor, start));
    if (emoji.url) {
      parts.push(<TwemojiImage key={`${start}-${emoji.text}`} text={emoji.text} url={emoji.url} />);
    } else {
      parts.push(emoji.text);
    }
    cursor = end;
  }
  if (cursor < children.length) parts.push(children.slice(cursor));
  return <>{parts}</>;
});

function normalizePreviewUrl(raw: string, serviceHost: string): string | undefined {
  if (!raw) return undefined;
  let url: URL;
  try { url = new URL(raw, location.origin); } catch { return undefined; }
  if (url.protocol !== "http:" && url.protocol !== "https:") return undefined;
  const localHost = serviceHost.toLowerCase().split(":")[0];
  const urlHost = canonicalServiceHost(url.hostname.toLowerCase());
  if (localHost && urlHost === localHost) {
    url = new URL(`${url.pathname}${url.search}`, location.origin);
  } else if (url.hostname.endsWith(".fbcdn.net") || url.hostname.endsWith(".cdninstagram.com")) {
    url.hostname = "scontent.cdninstagram.com";
  }
  return url.href;
}

function previewMediaUrl(
  raw: string,
  serviceHost: string,
  video = false,
  variant: "media" | "avatar" = "media"
): string | undefined {
  const normalized = normalizePreviewUrl(raw, serviceHost);
  if (!normalized) return undefined;
  const url = new URL(normalized);
  if (url.origin === location.origin && url.pathname.startsWith("/offload/")) {
    url.searchParams.set("preview", variant === "avatar" ? "avatar" : "1");
    if (video) url.searchParams.set("thumbnail", "1");
  }
  return url.href;
}

function safeHref(raw: string): string | null {
  try {
    const url = new URL(raw);
    return url.protocol === "http:" || url.protocol === "https:" || url.protocol === "mailto:" ? url.toString() : null;
  } catch { return null; }
}

function renderStatusNode(node: Node, key: string): React.ReactNode {
  if (node.nodeType === Node.TEXT_NODE) return <TwemojiText key={key}>{node.textContent ?? ""}</TwemojiText>;
  if (!(node instanceof HTMLElement)) return null;
  const children = Array.from(node.childNodes, (child, index) => renderStatusNode(child, `${key}-${index}`));
  switch (node.tagName.toLowerCase()) {
    case "br": return <br key={key} />;
    case "blockquote": return <blockquote key={key}>{children}</blockquote>;
    case "p": return <p key={key}>{children}</p>;
    case "b": case "strong": return <b key={key}>{children}</b>;
    case "i": case "em": return <i key={key}>{children}</i>;
    case "del": case "s": return <del key={key}>{children}</del>;
    case "a": {
      const href = safeHref(node.getAttribute("href") ?? "");
      return href ? <a key={key} href={href} target="_blank" rel="noreferrer noopener">{children}</a> : children;
    }
    default: return children;
  }
}

function renderStatusContent(input: string): React.ReactNode[] {
  const body = new DOMParser().parseFromString(input, "text/html").body;
  return Array.from(body.childNodes, (node, index) => renderStatusNode(node, `content-${index}`));
}

async function fetchStatusPreview(doc: Document, signal: AbortSignal, serviceHost: string, dateFormatter: Intl.DateTimeFormat): Promise<Partial<PreviewPost>> {
  const href = doc.querySelector('link[rel="alternate"][type="application/activity+json"]')?.getAttribute("href");
  const statusPath = href ? mastodonStatusPathFromAlternate(href, location.origin) : null;
  if (!statusPath) throw new Error("missing Mastodon status metadata");
  const response = await fetch(statusPath, { signal });
  if (!response.ok) throw new Error(`status ${response.status}`);
  const status = await response.json() as {
    content?: unknown;
    created_at?: unknown;
    media_attachments?: unknown;
    account?: { url?: unknown; avatar?: unknown };
  };
  if (typeof status.content !== "string") throw new Error("invalid Mastodon status");
  let captionNodes: React.ReactNode[] | undefined;
  if (status.content) {
    const nodes = renderStatusContent(status.content);
    if (nodes.length) captionNodes = nodes;
  }
  const media = Array.isArray(status.media_attachments) ? status.media_attachments.flatMap((raw): PreviewMedia[] => {
    if (!raw || typeof raw !== "object") return [];
    const item = raw as Record<string, unknown>;
    const kind = item.type === "image" ? "image" : item.type === "video" || item.type === "gifv" ? "video" : null;
    if (!kind || typeof item.url !== "string") return [];
    const mediaUrl = previewMediaUrl(item.url, serviceHost, kind === "video");
    if (!mediaUrl) return [];
    const original = (item.meta as Record<string, unknown> | undefined)?.original as Record<string, unknown> | undefined;
    return [{
      url: mediaUrl,
      kind,
      width: typeof original?.width === "number" && original.width > 0 ? original.width : undefined,
      height: typeof original?.height === "number" && original.height > 0 ? original.height : undefined,
    }];
  }).slice(0, 4) : [];
  const account = status.account;
  const created = typeof status.created_at === "string" ? new Date(status.created_at) : undefined;
  return {
    profileUrl: typeof account?.url === "string" ? safeHref(account.url) ?? undefined : undefined,
    authorIconUrl: typeof account?.avatar === "string"
      ? previewMediaUrl(account.avatar, serviceHost, false, "avatar")
      : undefined,
    captionNodes,
    media: media.length ? media : undefined,
    date: created && !Number.isNaN(created.getTime()) ? dateFormatter.format(created) : undefined,
  };
}

function previewDate(raw: unknown, dateFormatter: Intl.DateTimeFormat): string | undefined {
  if (typeof raw !== "string" || !raw) return undefined;
  const date = new Date(raw);
  return Number.isNaN(date.getTime()) ? undefined : dateFormatter.format(date);
}

async function parseHTMLPreview(doc: Document, signal: AbortSignal, serviceHost: string, dateFormatter: Intl.DateTimeFormat): Promise<Partial<PreviewPost>> {
  const meta = (key: string) => doc.querySelector(`meta[property="${key}"], meta[name="${key}"]`)?.getAttribute("content") ?? "";
  const title = meta("og:title");
  if (!title) throw new Error("no embed metadata");
  const status = await fetchStatusPreview(doc, signal, serviceHost, dateFormatter);
  const description = meta("og:description");
  const [firstBlock = "", ...restBlocks] = description.split("\n\n");
  const hasStats = STATS_EMOJI_RE.test(firstBlock);
  const isProfile = meta("og:type") === "profile";
  const isVideo = Boolean(meta("og:video"));
  const image = previewMediaUrl(meta("og:image"), serviceHost, isVideo);
  const authorIcon = previewMediaUrl(
    doc.querySelector('link[rel="apple-touch-icon"]')?.getAttribute("href") ?? "",
    serviceHost,
    false,
    "avatar"
  ) || (isProfile && image ? image : "");
  const fallbackMedia = image && !image.includes("/favicon-")
    ? [{ url: image, kind: isVideo ? "video" : "image" } satisfies PreviewMedia]
    : undefined;
  return {
    title,
    profileUrl: status.profileUrl ?? (safeHref(meta("article:author")) ?? undefined),
    authorIconUrl: status.authorIconUrl ?? (authorIcon || undefined),
    statsText: status.captionNodes ? undefined : (hasStats ? firstBlock : undefined),
    captionNodes: status.captionNodes,
    caption: status.captionNodes ? undefined : (hasStats ? restBlocks.join("\n\n") : description) || undefined,
    media: status.media ?? fallbackMedia,
    date: status.date ?? previewDate(meta("article:published_time"), dateFormatter),
  };
}

async function fetchPreview(path: string, token: string, signal: AbortSignal, serviceHost: string, dateFormatter: Intl.DateTimeFormat): Promise<Partial<PreviewPost>> {
  const response = await fetch("/api/embed", {
    method: "POST",
    headers: {
      "accept": "text/html",
      "content-type": "application/json",
    },
    body: JSON.stringify({ path, token }),
    signal,
  });
  if (response.status === 429) throw new Error("rate-limited");
  if (!response.ok) throw new Error(`status ${response.status}`);
  const contentType = response.headers.get("content-type")?.toLowerCase() ?? "";
  if (!contentType.includes("text/html")) throw new Error("invalid preview response");
  const doc = new DOMParser().parseFromString(await response.text(), "text/html");
  return parseHTMLPreview(doc, signal, serviceHost, dateFormatter);
}

function PreviewMediaGrid({ items = [], videoUnavailable }: { items?: PreviewMedia[]; videoUnavailable: string }) {
  if (!items.length) return null;
  const count = Math.min(items.length, 4);
  const layout = count === 1
    ? "grid-cols-1 max-w-[225px]"
    : count === 2
      ? "aspect-[2/1] max-w-[300px] grid-cols-2"
      : count === 3
        ? "aspect-[3/2] max-w-[300px] grid-cols-3 grid-rows-2"
        : "aspect-square max-w-[300px] grid-cols-2 grid-rows-2";
  return <div className={`mt-2.5 grid w-full gap-0.5 overflow-hidden rounded bg-[color-mix(in_srgb,var(--color-kumo-recessed)_70%,transparent)] ${layout}`}>
    {items.slice(0, 4).map((item, index) => <div key={`${item.url}-${index}`}
      className={`relative min-h-0 overflow-hidden ${count > 1 ? "h-full" : ""} ${count === 3 && index === 0 ? "col-span-2 row-span-2" : ""}`}>
      <img className={`block min-h-0 w-full object-cover saturate-[0.72] contrast-[1.02] ${count === 1 ? "h-auto" : "h-full"}`}
        src={item.url} alt="" width={item.width ?? 480} height={item.height ?? 480}
        style={count === 1 && item.width && item.height ? { aspectRatio: `${item.width} / ${item.height}` } : undefined}
        loading="lazy" decoding="async" />
      {item.kind === "video" ? <span className="absolute inset-0 flex flex-col items-center justify-center gap-1 bg-black/25 px-2 text-center text-white [text-shadow:0_1px_2px_rgb(0_0_0/0.5)]">
        <Play size={26} weight="fill" aria-hidden="true" />
        <span className="text-[0.65rem] leading-tight">{videoUnavailable}</span>
      </span> : null}
    </div>)}
  </div>;
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
    title: "Visit Türkiye (@visit-turkiye)",
    authorIconUrl: "/preview/v1/avatar-2.svg",
    statsText: "❤️ 12.4K 💬 84",
    caption: "Antalya, where the Taurus Mountains meet the Mediterranean.\nOld town lanes, turquoise coves, and the Red Tower glowing at sunset.\n\n#Antalya #Türkiye #TravelGuide",
    media: [{ url: "/preview/v1/antalya.webp", kind: "image", width: 480, height: 320 }],
    date: dateFormatter.format(new Date()),
  }), [dateFormatter, timeFormatter]);
  const [post, setPost] = useState<PreviewPost | null>(null);
  const postId = post?.id;
  const [stage, setStage] = useState<"sent" | "morph" | "embed">("sent");
  const [previewActive, setPreviewActive] = useState(true);
  const [inputValue, setInputValue] = useState("");
  const [typewriterState, setTypewriterState] = useState<"typing" | "paused" | "done">("typing");
  const [error, setError] = useState<string | null>(null);
  const [verifying, setVerifying] = useState(false);
  const composer = useRef<HTMLDivElement>(null);
  const typewriterText = useRef<HTMLSpanElement>(null);
  const typewriterCaret = useRef<HTMLSpanElement>(null);
  const nextPostId = useRef(1);
  const previewAbort = useRef<AbortController | null>(null);
  const turnstileHost = useRef<HTMLDivElement>(null);
  const turnstileWidget = useRef<string | null>(null);
  const pendingPath = useRef<string | null>(null);

  useEffect(() => {
    if (turnstileSiteKey) void loadTurnstile().catch(() => {});
  }, [turnstileSiteKey]);

  useEffect(() => () => {
    pendingPath.current = null;
    previewAbort.current?.abort();
    if (turnstileWidget.current !== null) {
      window.turnstile?.remove(turnstileWidget.current);
      turnstileWidget.current = null;
    }
  }, []);

  function cancelPreviewWork() {
    const hadPendingVerification = pendingPath.current !== null;
    pendingPath.current = null;
    if (hadPendingVerification && turnstileWidget.current !== null) {
      window.turnstile?.reset(turnstileWidget.current);
    }
    if (hadPendingVerification) setVerifying(false);
    previewAbort.current?.abort();
    previewAbort.current = null;
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
          pendingPath.current = null;
          if (!path) return;
          if (turnstileWidget.current !== null) window.turnstile?.reset(turnstileWidget.current);
          setVerifying(false);
          runPreview(path, token);
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

  useEffect(() => {
    if (postId === undefined || reduceMotion) return;
    const morphTimer = window.setTimeout(() => setStage("morph"), 900);
    const embedTimer = window.setTimeout(() => setStage("embed"), 1650);
    return () => {
      window.clearTimeout(morphTimer);
      window.clearTimeout(embedTimer);
    };
  }, [postId, reduceMotion]);

  function runPreview(path: string, token: string) {
    previewAbort.current?.abort();
    const id = nextPostId.current++;
    const controller = new AbortController();
    previewAbort.current = controller;
    setError(null);
    setInputValue("");
    setStage("sent");
    setPost({ id, author: copy.you, avatar: "/preview/v1/avatar-3.svg", time: timeFormatter.format(new Date()), path });
    fetchPreview(path, token, controller.signal, host, dateFormatter)
      .then((live) => {
        if (!controller.signal.aborted) setPost((current) => current && current.id === id ? { ...current, ...live } : current);
      })
      .catch((reason: unknown) => {
        if (controller.signal.aborted) return;
        setError(reason instanceof Error && reason.message === "rate-limited" ? copy.rateLimited : copy.fetchError);
      })
      .finally(() => {
        if (previewAbort.current === controller) previewAbort.current = null;
      });
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
    previewAbort.current?.abort();
    pendingPath.current = path;
    setError(null);
    setVerifying(true);
    void loadTurnstile().then(executeTurnstile).catch(failVerification);
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
      <div className="relative min-h-0 flex-1 overflow-y-auto overscroll-contain p-4 [scrollbar-width:thin]" aria-live="polite">
        <AnimatePresence mode="wait" initial={false}>
          {visiblePost ? <m.div key={visiblePost.id} className="grid grid-cols-[2.25rem_minmax(0,1fr)] gap-3 max-sm:grid-cols-[2rem_minmax(0,1fr)] max-sm:gap-2.5"
            initial={reduceMotion ? false : { opacity: 0, y: 14 }} animate={{ opacity: 1, y: 0 }}
            exit={reduceMotion ? undefined : { opacity: 0, y: -10, transition: { duration: 0.16 } }} transition={{ type: "spring", stiffness: 320, damping: 26 }}>
            <img className="h-9 w-9 rounded-full border border-[color-mix(in_srgb,var(--color-kumo-brand)_20%,var(--color-kumo-hairline))] bg-[color-mix(in_srgb,var(--color-kumo-brand)_9%,var(--color-kumo-base))] object-cover max-sm:h-8 max-sm:w-8"
              src={visiblePost.avatar} alt="" width={36} height={36} aria-hidden="true" />
            <div className="min-w-0">
              <div className="flex items-baseline gap-2 leading-tight"><strong className="text-sm font-semibold text-kumo-default" translate="no">{visiblePost.author}</strong><span className="text-[0.67rem] text-kumo-subtle">{visiblePost.time}</span></div>
              <div className="relative mt-1.5 h-[1.4rem] overflow-hidden text-[0.8rem] leading-[1.4rem] max-sm:text-xs" translate="no">
                <div className={`preview-url-source absolute inset-0 truncate text-kumo-subtle ${visibleStage === "sent" ? "" : "preview-url-hidden"}`}>https://{baseHost}{visiblePost.path}</div>
                <div className={`preview-url-target absolute inset-0 truncate text-kumo-default ${visibleStage === "sent" ? "" : "preview-url-visible"}`}><span>https://</span><HighlightedHost host={host} /><span>{visiblePost.path}</span></div>
              </div>
              {visibleStage === "embed" && visiblePost.title ? <m.article className="mt-3 rounded border-l-4 border-kumo-brand bg-kumo-base py-2 pr-4 pb-4 pl-3 [overflow-wrap:anywhere] max-sm:pr-3"
                initial={reduceMotion ? false : { opacity: 0, y: 12 }} animate={{ opacity: 1, y: 0 }} transition={{ type: "spring", stiffness: 260, damping: 26 }}>
                <div className="mt-2 flex items-center gap-2">
                  <img className="h-6 w-6 rounded-full object-cover" src={visiblePost.authorIconUrl ?? visiblePost.avatar} alt="" width={24} height={24} />
                  {visiblePost.profileUrl ? <a className="text-sm font-semibold text-kumo-default no-underline hover:underline" href={visiblePost.profileUrl} target="_blank" rel="noreferrer noopener" translate="no">{visiblePost.title}</a> : <span className="text-sm font-semibold text-kumo-default" translate="no">{visiblePost.title}</span>}
                </div>
                {visiblePost.statsText || visiblePost.captionNodes || visiblePost.caption ? <div className="mt-2 flex flex-col gap-2 text-[0.8rem] leading-[1.45] text-kumo-default max-sm:text-xs">
                  {visiblePost.statsText ? <p><strong translate="no"><TwemojiText>{visiblePost.statsText}</TwemojiText></strong></p> : null}
                  {visiblePost.captionNodes ? <div className="[&_a]:text-kumo-brand [&_a]:underline [&_a]:underline-offset-2 [&_p]:whitespace-pre-wrap [&_p+p]:mt-2 [&_blockquote]:border-l-2 [&_blockquote]:border-kumo-line [&_blockquote]:pl-2">{visiblePost.captionNodes}</div> : visiblePost.caption ? <p className="whitespace-pre-line"><TwemojiText>{visiblePost.caption}</TwemojiText></p> : null}
                </div> : null}
                <PreviewMediaGrid items={visiblePost.media} videoUnavailable={copy.videoUnavailable} />
                <div className="mt-2.5 flex items-center gap-2"><img className="h-5 w-5 rounded-full object-cover" src="/favicon-64.png" alt="" width={20} height={20} /><span className="text-[0.7rem] leading-tight text-kumo-subtle" translate="no">{brand}{visiblePost.date ? <><i className="mx-1 not-italic">•</i>{visiblePost.date}</> : null}</span></div>
              </m.article> : null}
            </div>
          </m.div> : null}
        </AnimatePresence>
      </div>
      <form className="flex flex-none flex-wrap items-start gap-2 border-t border-[color-mix(in_srgb,var(--color-kumo-hairline)_86%,transparent)] bg-[color-mix(in_srgb,var(--color-kumo-recessed)_55%,var(--color-kumo-elevated))] p-3 pb-3.5" onSubmit={submitLink}>
        <div ref={composer} className="composer-pulse relative min-w-0 flex-1 rounded-lg [&_label]:hidden">
          <Input className={`w-full ${previewActive && !reduceMotion && typewriterState !== "done" ? "text-transparent caret-transparent placeholder:text-transparent" : ""}`}
            value={inputValue} onChange={(event) => {
              cancelPreviewWork();
              setInputValue(event.target.value);
              setPreviewActive(false);
              if (error) setError(null);
            }}
            onFocus={() => {
              if (previewActive) {
                cancelPreviewWork();
                setPreviewActive(false);
                setInputValue("");
              }
            }} placeholder={previewActive && !reduceMotion && typewriterState !== "done" ? "" : copy.placeholder}
            aria-label="Instagram URL" error={error ?? undefined} translate="no" passwordManagerIgnore autoComplete="off" spellCheck={false} inputMode="url" enterKeyHint="go" />
          {previewActive && !reduceMotion && typewriterState !== "done" ? <span aria-hidden="true" className="pointer-events-none absolute inset-y-0 left-3 right-3 flex items-center overflow-hidden whitespace-pre text-base text-kumo-default">
            <span className="relative inline-block max-w-full">
              <span ref={typewriterText} className="typewriter-text block truncate">{`https://${baseHost}${samplePost.path}`}</span>
              <span ref={typewriterCaret} className={`typewriter-caret absolute top-[0.1em] left-[0.2em] inline-block h-[1em] w-[2px] bg-kumo-brand ${typewriterState === "paused" ? "typewriter-caret-blink" : ""}`} />
            </span>
          </span> : null}
        </div>
        <Button type="submit" variant="primary" shape="square" icon={PaperPlaneRight} aria-label={copy.submit} disabled={!turnstileSiteKey || verifying} />
        <div ref={turnstileHost} className="basis-full empty:hidden" />
      </form>
    </div>
  </figure>;
}

export default React.memo(Preview);
