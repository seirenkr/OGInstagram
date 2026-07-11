import React, { useEffect, useMemo, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import { Banner } from "@cloudflare/kumo/components/banner";
import { Button, LinkButton } from "@cloudflare/kumo/components/button";
import { Grid, GridItem } from "@cloudflare/kumo/components/grid";
import { Input } from "@cloudflare/kumo/components/input";
import { LayerCard } from "@cloudflare/kumo/components/layer-card";
import { DropdownMenu } from "@cloudflare/kumo/components/dropdown";
import { Text } from "@cloudflare/kumo/components/text";
import { Tooltip, TooltipProvider } from "@cloudflare/kumo/components/tooltip";
import { KumoPortalProvider } from "@cloudflare/kumo/utils";
import { ChartLegend, ChartPalette, TimeseriesChart } from "@cloudflare/kumo/components/chart";
import { Clock, Coffee, GithubLogo, Info, Moon, PaperPlaneRight, Play, Sun, Translate, WarningCircle } from "@phosphor-icons/react";
import { AnimatePresence, motion, useAnimate, useReducedMotion } from "motion/react";
import * as echarts from "echarts/core";
import { LineChart } from "echarts/charts";
import { AriaComponent, AxisPointerComponent, BrushComponent, GridComponent, LegendComponent, ToolboxComponent, TooltipComponent } from "echarts/components";
import { CanvasRenderer } from "echarts/renderers";
import "./styles.css";

echarts.use([LineChart, AxisPointerComponent, BrushComponent, GridComponent, LegendComponent, ToolboxComponent, TooltipComponent, AriaComponent, CanvasRenderer]);

const CHART_FONT_FAMILY = '"PP Mori", "Pretendard JP Variable", Pretendard, ui-sans-serif, system-ui, sans-serif';
const chartEcharts = {
  ...echarts,
  init: (...args: Parameters<typeof echarts.init>) => {
    const chart = echarts.init(...args);
    chart.setOption({ useUTC: false, textStyle: { fontFamily: CHART_FONT_FAMILY } });
    return chart;
  },
} as typeof echarts;

type Copy = {
  successful: string; failed: string; restricted: string; avg: string; ms: string;
  noDataYet: string; statsUnavailable: string; skipToContent: string; timeUTC: string; language: string;
};

type HeroCopy = {
  line1: string; line2: string; rich: string; channel: string;
  placeholder: string; invalid: string; fetchError: string; rateLimited: string; submit: string; demoDesc: string; you: string;
  videoUnavailable: string;
};

declare global {
  interface Window {
    turnstile?: {
      render: (element: HTMLElement, options: {
        sitekey: string; action: string; appearance: "execute"; execution: "execute";
        callback: (token: string, preClearanceObtained: boolean) => void; "error-callback": () => void;
      }) => string;
      execute: (widgetId: string) => void;
      reset: (widgetId: string) => void;
    };
    onloadTurnstileCallback?: () => void;
  }
}

type AppData = {
  brand: string; version: string; host: string; lang: string; tagline: string; hero: HeroCopy; turnstileSiteKey: string;
  supportURL: string; supportCta: string; githubURL: string; darkMode: string; lightMode: string;
  usageH2: string; normalView: string; normalDesc: string; galleryView: string;
  galleryDesc: string; directView: string; directDesc: string; supportedH2: string;
  supportNote: string; posts: string; userProfile: string; reels: string;
  statusH2: string; statusSub: string; requests: string; responseTime: string;
  disclaimer: string; js: Copy;
};

type Status = { t?: number[]; resolved?: number[]; restricted?: number[]; failed?: number[]; latency?: number[] };

const data = JSON.parse(document.getElementById("app-data")!.textContent!) as AppData;
const languages = { en: "English", es: "Español", fr: "Français", ja: "日本語", ko: "한국어", pt: "Português", "zh-hans": "简体中文", "zh-hant": "繁體中文" };
const EMPTY_VALUES: number[] = [];
const browserTimeZone = Intl.DateTimeFormat().resolvedOptions().timeZone || "Local";
const chartTimeLabel = data.js.timeUTC.replace("UTC", browserTimeZone);

function sum(values: number[] = []) { return values.reduce((total, value) => total + value, 0); }
function latest(values: number[] = []) { return values.length ? values[values.length - 1] : 0; }
const numberFormatter = new Intl.NumberFormat(data.lang, { maximumFractionDigits: 0 });
function format(value: number) { return numberFormatter.format(Math.round(value)); }
const timeFormatter = new Intl.DateTimeFormat(data.lang, { hour: "numeric", minute: "numeric" });
const dateFormatter = new Intl.DateTimeFormat(data.lang, { year: "numeric", month: "numeric", day: "numeric" });

// Random per page load so demo avatars vary, stable within the session so
// re-renders don't swap faces mid-conversation.
function dicebearURL() {
  return `https://api.dicebear.com/10.x/dylan/svg?seed=${crypto.randomUUID()}`;
}

function HighlightedHost({ host }: { host: string }) {
  if (!host.toLowerCase().startsWith("og")) return host;
  return <><strong className="text-kumo-brand">{host.slice(0, 2)}</strong>{host.slice(2)}</>;
}

// The last 10-minute bucket is still being aggregated; dash it as incomplete.
function incompleteAfter(times: number[]): { after: number } | undefined {
  return times.length >= 2 ? { after: times[times.length - 2] * 1000 } : undefined;
}

const EASE = [0.16, 1, 0.3, 1] as const;
const heroItem = { hidden: { opacity: 0, y: 10 }, show: { opacity: 1, y: 0, transition: { duration: 0.48, ease: EASE } } };
const heroLine = { hidden: { y: "112%" }, show: { y: "0%", transition: { duration: 0.65, ease: EASE } } };

// Fade + rise a whole section as it scrolls into view. Degrades to static
// when the visitor prefers reduced motion.
function Reveal({ children, className, reduceMotion, ...rest }: React.ComponentProps<typeof motion.section> & { reduceMotion: boolean }) {
  return <motion.section className={className}
    initial={reduceMotion ? false : { opacity: 0, y: 24 }}
    whileInView={{ opacity: 1, y: 0 }}
    viewport={{ once: true, amount: 0.2 }}
    transition={{ duration: 0.6, ease: EASE }} {...rest}>{children}</motion.section>;
}

// Subtle lift on hover so cards and buttons feel tactile, not painted on.
const lift = "transition-transform duration-150 ease-out hover:-translate-y-[3px] motion-reduce:transition-none";

// Discord renders emoji as Twemoji images; match it in the embed stats.
const TWEMOJI_CODES: Record<string, string> = { "❤️": "2764", "💬": "1f4ac", "📝": "1f4dd", "👤": "1f464", "▶️": "25b6" };
const STATS_EMOJI_RE = /❤️|💬|📝|👤|▶️/;

function StatsLine({ text }: { text: string }) {
  const parts = text.split(/(❤️|💬|📝|👤|▶️)/);
  return <strong translate="no">{parts.map((part, index) => TWEMOJI_CODES[part]
    ? <img key={index} className="inline-block h-[1.1em] w-[1.1em] align-[-0.18em]" alt={part} draggable={false}
        src={`https://cdnjs.cloudflare.com/ajax/libs/twemoji/14.0.2/svg/${TWEMOJI_CODES[part]}.svg`} width={20} height={20} />
    : part)}</strong>;
}

function parseInstagramLink(raw: string): string | null {
  let value = raw.trim();
  if (!value) return null;
  if (!/^https?:\/\//i.test(value)) value = `https://${value}`;
  let url: URL;
  try { url = new URL(value); } catch { return null; }
  const hostname = url.hostname.toLowerCase();
  if (hostname !== "instagram.com" && !hostname.endsWith(".instagram.com")) return null;
  const parts = url.pathname.split("/").filter(Boolean);
  if (!parts.length) return null;
  const path = `/${parts.join("/")}`;
  const mediaIndex = parts.findIndex((part) => ["p", "reel", "reels"].includes(part.toLowerCase()));
  if (mediaIndex === -1) return parts.length === 1 ? path : null;
  return parts[mediaIndex + 1] ? path : null;
}

type PreviewPost = {
  id: number; author: string; avatar: string; time: string; path: string;
  title?: string; profileUrl?: string; authorIconUrl?: string;
  statsText?: string; caption?: string; captionHTML?: string; media?: PreviewMedia[]; date?: string;
};

type PreviewMedia = { url: string; kind: "image" | "video"; width?: number; height?: number };

function normalizePreviewURL(raw: string, localizeService = false): string | undefined {
  if (!raw) return undefined;
  let url: URL;
  try { url = new URL(raw, location.origin); } catch { return undefined; }
  if (url.protocol !== "http:" && url.protocol !== "https:") return undefined;

  const serviceHost = data.host.toLowerCase().replace(/^www\./, "").split(":")[0];
  const urlHost = url.hostname.toLowerCase().replace(/^www\./, "");
  if (localizeService && serviceHost && urlHost === serviceHost) {
    url = new URL(`${url.pathname}${url.search}`, location.origin);
  } else if (url.hostname.endsWith(".fbcdn.net") || url.hostname.endsWith(".cdninstagram.com")) {
    url.hostname = "scontent.cdninstagram.com";
  }
  return url.href;
}

function videoPosterURL(raw: string): string | undefined {
  const normalized = normalizePreviewURL(raw, true);
  if (!normalized) return undefined;
  const url = new URL(normalized);
  url.searchParams.set("thumbnail", "1");
  return url.href;
}

const DEMO_POST: PreviewPost = {
  id: 0, author: "Phibi", avatar: dicebearURL(), time: timeFormatter.format(new Date()), path: "/p/ExAmpl3",
  title: "Visit Türkiye (@visit-turkiye)", authorIconUrl: dicebearURL(),
  statsText: "❤️ 12.4K 💬 84",
  caption: "Antalya, where the Taurus Mountains meet the Mediterranean.\nOld town lanes, turquoise coves, and the Red Tower glowing at sunset.\n\n#Antalya #Türkiye #TravelGuide",
  media: [{ url: "https://images.pexels.com/photos/12940603/pexels-photo-12940603.jpeg?auto=compress&cs=tinysrgb&w=480", kind: "image", width: 480, height: 320 }],
  date: dateFormatter.format(new Date()),
};

async function fetchActivityPreview(doc: Document): Promise<Partial<PreviewPost>> {
  const href = doc.querySelector('link[rel="alternate"][type="application/activity+json"]')?.getAttribute("href");
  if (!href) return {};
  let url: URL;
  try {
    const canonical = new URL(href, location.origin);
    if (canonical.protocol !== "http:" && canonical.protocol !== "https:") return {};
    url = new URL(`${canonical.pathname}${canonical.search}`, location.origin);
  } catch { return {}; }

  try {
    const response = await fetch(url);
    if (!response.ok) return {};
    const note = await response.json() as { content?: unknown; attachment?: unknown };
    let statsText: string | undefined;
    let captionHTML: string | undefined;
    if (typeof note.content === "string" && note.content) {
      // This HTML is generated by our same-origin ActivityPub renderer, which
      // escapes captions before adding its own p/b/a/br elements.
      const content = new DOMParser().parseFromString(note.content, "text/html");
      const first = content.body.firstElementChild;
      const stats = first?.firstElementChild;
      if (first?.tagName === "P" && (stats?.tagName === "B" || stats?.tagName === "STRONG")) {
        statsText = first.textContent?.trim() || undefined;
        first.remove();
      }
      for (const anchor of content.body.querySelectorAll("a")) {
        anchor.target = "_blank";
        anchor.rel = "noreferrer noopener";
      }
      captionHTML = content.body.innerHTML.trim() || undefined;
    }

    const media = Array.isArray(note.attachment) ? note.attachment.flatMap((raw): PreviewMedia[] => {
      if (!raw || typeof raw !== "object") return [];
      const item = raw as Record<string, unknown>;
      if (typeof item.url !== "string" || typeof item.mediaType !== "string") return [];
      const kind = item.mediaType.startsWith("image/") ? "image" : item.mediaType.startsWith("video/") ? "video" : null;
      if (!kind) return [];
      const mediaURL = kind === "video" ? videoPosterURL(item.url) : normalizePreviewURL(item.url, true);
      if (!mediaURL) return [];
      return [{
        url: mediaURL,
        kind,
        width: typeof item.width === "number" && item.width > 0 ? item.width : undefined,
        height: typeof item.height === "number" && item.height > 0 ? item.height : undefined,
      }];
    }).slice(0, 4) : [];
    return { statsText, captionHTML, media: media.length ? media : undefined };
  } catch {
    return {};
  }
}

// Fetch the same-origin embed HTML and read the Open Graph tags Discord would.
async function fetchLivePreview(path: string): Promise<Partial<PreviewPost>> {
  const res = await fetch("/api/embed", {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ path }),
  });
  if (res.status === 429) throw new Error("rate-limited");
  // The WAF managed challenge marks blocked fetches with cf-mitigated.
  if (res.status === 403 && res.headers.get("cf-mitigated") === "challenge") throw new Error("verification-required");
  if (!res.ok) throw new Error(`status ${res.status}`);
  const doc = new DOMParser().parseFromString(await res.text(), "text/html");
  const meta = (key: string) => doc.querySelector(`meta[property="${key}"], meta[name="${key}"]`)?.getAttribute("content") ?? "";
  const title = meta("og:title");
  if (!title) throw new Error("no embed metadata");
  const description = meta("og:description");
  const [firstBlock = "", ...restBlocks] = description.split("\n\n");
  const hasStats = STATS_EMOJI_RE.test(firstBlock);
  const image = normalizePreviewURL(meta("og:image"), true);
  const isProfile = meta("og:type") === "profile";
  const isVideo = Boolean(meta("og:video"));
  const published = meta("article:published_time");
  const publishedDate = published ? new Date(published) : new Date();
  // Profile embeds carry the avatar as og:image; reuse it for the author row.
  const authorIcon = normalizePreviewURL(doc.querySelector('link[rel="apple-touch-icon"]')?.getAttribute("href") ?? "", true) || (isProfile && image ? image : "");
  const activity = await fetchActivityPreview(doc);
  const fallbackMedia = image && !image.includes("/favicon-")
    ? [{ url: image, kind: isVideo ? "video" : "image" } satisfies PreviewMedia]
    : undefined;
  return {
    title,
    profileUrl: meta("article:author") || undefined,
    authorIconUrl: authorIcon || undefined,
    statsText: activity.statsText ?? (hasStats ? firstBlock : undefined),
    captionHTML: activity.captionHTML,
    caption: (hasStats ? restBlocks.join("\n\n") : description) || undefined,
    media: activity.media ?? fallbackMedia,
    date: dateFormatter.format(Number.isNaN(publishedDate.getTime()) ? new Date() : publishedDate),
  };
}

function PreviewMediaGrid({ items = [] }: { items?: PreviewMedia[] }) {
  if (!items.length) return null;
  const count = Math.min(items.length, 4);
  // 1 keeps the original aspect ratio; 2/4 are square cells; 3 is one big
  // square with two small squares stacked beside it.
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
        <span className="text-[0.65rem] leading-tight">{data.hero.videoUnavailable}</span>
      </span> : null}
    </div>)}
  </div>;
}

// Demo plays once unless the visitor starts using the composer first.
function LivePreview({ brand, host, reduceMotion }: { brand: string; host: string; reduceMotion: boolean }) {
  const t = data.hero;
  const baseHost = host.toLowerCase().startsWith("og") ? host.slice(2) : host;
  const [post, setPost] = useState<PreviewPost | null>(null);
  const [stage, setStage] = useState<"sent" | "morph" | "embed">("sent");
  const [demoActive, setDemoActive] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [composerScope, animateComposer] = useAnimate();
  const inputRef = useRef<HTMLInputElement>(null);
  const nextPostId = useRef(1);
  const turnstileHost = useRef<HTMLDivElement>(null);
  const turnstileWidget = useRef<string | null>(null);
  const pendingPath = useRef<string | null>(null);

  useEffect(() => {
    if (!data.turnstileSiteKey) return;
    const renderWidget = () => {
      if (!window.turnstile || !turnstileHost.current || turnstileWidget.current !== null) return;
      turnstileWidget.current = window.turnstile.render(turnstileHost.current, {
        sitekey: data.turnstileSiteKey,
        action: "turnstile-spin-v1",
        appearance: "execute",
        execution: "execute",
        callback: (_token, preClearanceObtained) => {
          const path = pendingPath.current;
          pendingPath.current = null;
          if (turnstileWidget.current) window.turnstile?.reset(turnstileWidget.current);
          // The widget mints the cf_clearance cookie (pre-clearance); the
          // retried fetch now passes the WAF. The token itself is unused.
          if (path && preClearanceObtained) runPreview(path, true);
          else if (path) setError(t.fetchError);
        },
        "error-callback": () => {
          pendingPath.current = null;
          setError(t.fetchError);
        },
      });
    };
    // api.js loads with ?onload=onloadTurnstileCallback; render immediately if
    // it beat us here, otherwise when it fires the callback.
    window.onloadTurnstileCallback = renderWidget;
    renderWidget();
    return () => { window.onloadTurnstileCallback = undefined; };
  }, []);

  useEffect(() => {
    if (!demoActive) return;
    if (reduceMotion) { setPost(DEMO_POST); return; }
    const input = inputRef.current;
    if (!input) return;
    const source = `https://${baseHost}${DEMO_POST.path}`;
    const timers: number[] = [];
    let typing = 0;
    timers.push(window.setTimeout(() => {
      let length = 0;
      typing = window.setInterval(() => {
        length += 1;
        input.value = source.slice(0, length);
        if (length >= source.length) {
          window.clearInterval(typing);
          timers.push(window.setTimeout(() => { input.value = ""; setPost(DEMO_POST); }, 460));
        }
      }, 26);
    }, 700));
    return () => { timers.forEach(window.clearTimeout); window.clearInterval(typing); };
  }, [baseHost, demoActive, reduceMotion]);

  useEffect(() => {
    if (!post) return;
    if (reduceMotion) { setStage("embed"); return; }
    const timers = [
      window.setTimeout(() => setStage("morph"), 900),
      window.setTimeout(() => setStage("embed"), 1650),
    ];
    return () => timers.forEach(window.clearTimeout);
  }, [post?.id, reduceMotion]);

  function runPreview(path: string, retried = false) {
    const id = nextPostId.current++;
    pendingPath.current = null;
    setError(null);
    if (inputRef.current) inputRef.current.value = "";
    setStage("sent");
    setPost({ id, author: t.you, avatar: dicebearURL(), time: timeFormatter.format(new Date()), path });
    fetchLivePreview(path)
      .then((live) => setPost((current) => current && current.id === id ? { ...current, ...live } : current))
      .catch((err: Error) => {
        // A challenged fetch means no cf_clearance yet: solve the Turnstile
        // challenge once, then retry. A second challenge in a row means the
        // clearance didn't take; surface an error instead of looping.
        if (err.message === "verification-required" && !retried && turnstileWidget.current) {
          pendingPath.current = path;
          window.turnstile?.execute(turnstileWidget.current);
          return;
        }
        setError(err.message === "rate-limited" ? t.rateLimited : t.fetchError);
      });
  }

  function submitLink(event: React.FormEvent) {
    event.preventDefault();
    setDemoActive(false);
    const path = parseInstagramLink(inputRef.current?.value ?? "");
    if (!path) {
      setError(t.invalid);
      if (!reduceMotion && composerScope.current) animateComposer(composerScope.current, { x: [0, -7, 7, -5, 5, 0] }, { duration: 0.36 });
      return;
    }
    runPreview(path);
  }

  return <figure className="m-0 w-full">
    <figcaption className="sr-only">{t.demoDesc}</figcaption>
    <div className="flex h-[32rem] flex-col overflow-hidden rounded-[14px] border border-kumo-hairline bg-kumo-elevated text-kumo-default shadow-[0_30px_80px_-48px_var(--color-kumo-shadow-drop)]">
      <div className="group flex h-[2.4rem] flex-none items-center gap-3 border-b border-[color-mix(in_srgb,var(--color-kumo-hairline)_86%,transparent)] bg-[linear-gradient(180deg,color-mix(in_srgb,var(--color-kumo-base)_62%,var(--color-kumo-elevated)),color-mix(in_srgb,var(--color-kumo-recessed)_72%,var(--color-kumo-elevated)))] px-3.5 max-sm:h-[2.125rem] max-sm:px-3" aria-hidden="true">
        <div className="flex gap-[0.44rem]">
          {["bg-[#ff5f57]", "bg-[#febc2e] group-hover:delay-[45ms]", "bg-[#28c840] group-hover:delay-[90ms]"].map((extra) => (
            <span key={extra} className={`h-2.5 w-2.5 rounded-full border border-black/15 shadow-[inset_0_1px_0_rgb(255_255_255/0.28),0_0.5px_0_rgb(0_0_0/0.08)] transition-transform duration-150 group-hover:scale-120 motion-reduce:transition-none ${extra}`} />
          ))}
        </div>
        <span className="text-xs font-semibold text-kumo-subtle" translate="no">{t.channel}</span>
      </div>
      <div className="relative min-h-0 flex-1 overflow-y-auto overscroll-contain p-4 [scrollbar-width:thin]" aria-live="polite">
        <AnimatePresence mode="wait" initial={false}>
          {post ? <motion.div key={post.id} className="grid grid-cols-[2.25rem_minmax(0,1fr)] gap-3 max-sm:grid-cols-[2rem_minmax(0,1fr)] max-sm:gap-2.5"
            initial={reduceMotion ? false : { opacity: 0, y: 14 }}
            animate={{ opacity: 1, y: 0 }}
            exit={reduceMotion ? undefined : { opacity: 0, y: -10, transition: { duration: 0.16 } }}
            transition={{ type: "spring", stiffness: 320, damping: 26 }}>
            <img className="h-9 w-9 rounded-full border border-[color-mix(in_srgb,var(--color-kumo-brand)_20%,var(--color-kumo-hairline))] bg-[color-mix(in_srgb,var(--color-kumo-brand)_9%,var(--color-kumo-base))] object-cover max-sm:h-8 max-sm:w-8"
              src={post.avatar} alt="" width={36} height={36} aria-hidden="true" />
            <div className="min-w-0">
              <div className="flex items-baseline gap-2 leading-tight">
                <strong className="text-sm font-semibold text-kumo-default" translate="no">{post.author}</strong>
                <span className="text-[0.67rem] text-kumo-subtle">{post.time}</span>
              </div>
              {/* URL line inherits the page sans stack on purpose (no mono). */}
              <div className="relative mt-1.5 h-[1.4rem] overflow-hidden text-[0.8rem] leading-[1.4rem] max-sm:text-xs" translate="no">
                <motion.div className="absolute inset-0 truncate text-kumo-subtle" initial={false}
                  animate={{ opacity: stage === "sent" ? 1 : 0, y: stage === "sent" ? 0 : -7 }}
                  transition={{ duration: 0.4, ease: EASE }}>https://{baseHost}{post.path}</motion.div>
                <motion.div className="absolute inset-0 truncate text-kumo-default" initial={false}
                  animate={{ opacity: stage === "sent" ? 0 : 1, y: stage === "sent" ? 7 : 0 }}
                  transition={{ duration: 0.45, ease: EASE }}><span>https://</span><HighlightedHost host={host} /><span>{post.path}</span></motion.div>
              </div>
              {stage === "embed" && post.title ? <motion.article className="mt-3 rounded border-l-4 border-kumo-brand bg-kumo-base py-2 pr-4 pb-4 pl-3 [overflow-wrap:anywhere] max-sm:pr-3"
                initial={reduceMotion ? false : { opacity: 0, y: 12 }}
                animate={{ opacity: 1, y: 0 }}
                transition={{ type: "spring", stiffness: 260, damping: 26 }}>
                <div className="mt-2 flex items-center gap-2">
                  <img className="h-6 w-6 rounded-full object-cover" src={post.authorIconUrl ?? post.avatar} alt="" width={24} height={24} />
                  {post.profileUrl
                    ? <a className="text-sm font-semibold text-kumo-default no-underline hover:underline" href={post.profileUrl} target="_blank" rel="noreferrer noopener" translate="no">{post.title}</a>
                    : <span className="text-sm font-semibold text-kumo-default" translate="no">{post.title}</span>}
                </div>
                {post.statsText || post.captionHTML || post.caption ? <div className="mt-2 flex flex-col gap-2 text-[0.8rem] leading-[1.45] text-kumo-default max-sm:text-xs">
                  {post.statsText ? <p><StatsLine text={post.statsText} /></p> : null}
                  {post.captionHTML
                    ? <div className="[&_a]:text-kumo-brand [&_a]:underline [&_a]:underline-offset-2 [&_p]:whitespace-pre-wrap [&_p+p]:mt-2" dangerouslySetInnerHTML={{ __html: post.captionHTML }} />
                    : post.caption ? <p className="whitespace-pre-line">{post.caption}</p> : null}
                </div> : null}
                <PreviewMediaGrid items={post.media} />
                <div className="mt-2.5 flex items-center gap-2">
                  <img className="h-5 w-5 rounded-full object-cover" src="/favicon-64.png" alt="" width={20} height={20} />
                  <span className="text-[0.7rem] leading-tight text-kumo-subtle" translate="no">{brand}{post.date ? <><i className="mx-1 not-italic">•</i>{post.date}</> : null}</span>
                </div>
              </motion.article> : null}
            </div>
          </motion.div> : null}
        </AnimatePresence>
      </div>
      <form className="flex flex-none flex-wrap items-start gap-2 border-t border-[color-mix(in_srgb,var(--color-kumo-hairline)_86%,transparent)] bg-[color-mix(in_srgb,var(--color-kumo-recessed)_55%,var(--color-kumo-elevated))] p-3 pb-3.5" onSubmit={submitLink}>
        <div ref={composerScope} className="min-w-0 flex-1 rounded-lg sm:motion-safe:animate-composer-ready [&_label]:hidden">
          <Input ref={inputRef} className="w-full"
            onChange={() => { setDemoActive(false); if (error) setError(null); }}
            placeholder={t.placeholder}
            aria-label="Instagram URL"
            error={error ?? undefined} translate="no" passwordManagerIgnore
            autoComplete="off" spellCheck={false} inputMode="url" enterKeyHint="go" />
        </div>
        <Button type="submit" variant="primary" shape="square" icon={PaperPlaneRight} aria-label={t.submit} disabled={!data.turnstileSiteKey} />
        <div ref={turnstileHost} className="basis-full empty:hidden" />
      </form>
    </div>
  </figure>;
}

function UsageCard({ title, url, description }: { title: string; url: React.ReactNode; description: string }) {
  return <LayerCard className="h-full">
    <LayerCard.Secondary>{title}</LayerCard.Secondary>
    <LayerCard.Primary className="grow">
      <div className="flex flex-col gap-3">
        <div className="break-words"><Text>{url}</Text></div>
        <Text variant="secondary">{description}</Text>
      </div>
    </LayerCard.Primary>
  </LayerCard>;
}

function App() {
  const reduceMotion = useReducedMotion() ?? false;
  const [dark, setDark] = useState(document.documentElement.dataset.mode === "dark");
  const [scrolled, setScrolled] = useState(window.scrollY > 8);
  const topSentinel = useRef<HTMLDivElement>(null);
  const overlayPortal = useRef<HTMLDivElement>(null);
  const [status, setStatus] = useState<Status | null>(null);
  const [statusFailed, setStatusFailed] = useState(false);
  useEffect(() => {
    fetch("/_status").then((response) => {
      if (!response.ok) throw new Error("status request failed");
      return response.json() as Promise<Status>;
    }).then(setStatus).catch(() => setStatusFailed(true));
  }, []);
  // Header state driven by a top sentinel via IntersectionObserver, not a
  // per-frame scroll listener (taste skill 5.D bans window scroll listeners).
  useEffect(() => {
    const el = topSentinel.current;
    if (!el) return;
    const io = new IntersectionObserver(([entry]) => setScrolled(!entry.isIntersecting));
    io.observe(el);
    return () => io.disconnect();
  }, []);
  function setTheme(nextDark: boolean) {
    const apply = () => {
      document.documentElement.dataset.mode = nextDark ? "dark" : "light";
      localStorage.setItem("theme", nextDark ? "dark" : "light");
      setDark(nextDark);
    };
    // Native cross-fade via the View Transitions API; unsupported browsers
    // and reduced-motion users get the instant switch as before.
    if (reduceMotion || !document.startViewTransition) apply();
    else document.startViewTransition(apply);
  }
  const times = status?.t ?? EMPTY_VALUES;
  const resolved = status?.resolved ?? EMPTY_VALUES;
  const restricted = status?.restricted ?? EMPTY_VALUES;
  const failed = status?.failed ?? EMPTY_VALUES;
  const latency = status?.latency ?? EMPTY_VALUES;
  const successColor = ChartPalette.semantic("Success", dark);
  const restrictedColor = ChartPalette.semantic("Warning", dark);
  const errorColor = ChartPalette.semantic("Attention", dark);
  const requestSeries = useMemo(() => [
    { name: data.js.successful, color: successColor, data: times.map((time, index) => [time * 1000, resolved[index] ?? 0] as [number, number]) },
    { name: data.js.restricted, color: restrictedColor, data: times.map((time, index) => [time * 1000, restricted[index] ?? 0] as [number, number]) },
    { name: data.js.failed, color: errorColor, data: times.map((time, index) => [time * 1000, failed[index] ?? 0] as [number, number]) },
  ], [times, resolved, restricted, failed, successColor, restrictedColor, errorColor]);
  const requestsChartRef = useRef<echarts.ECharts>(null);
  // Name of the isolated series, or null when all are visible.
  const [isolated, setIsolated] = useState<string | null>(null);
  // A theme switch re-inits the ECharts instance, resetting legend selection
  // to all-visible; reset our state to match so the legend doesn't desync.
  useEffect(() => { setIsolated(null); }, [dark]);
  // Click isolates a series via the hidden ECharts legend; clicking the
  // already-isolated series restores all.
  function toggleSeries(name: string) {
    const chart = requestsChartRef.current;
    if (!chart) return;
    const restore = isolated === name;
    for (const series of requestSeries) {
      chart.dispatchAction({ type: restore || series.name === name ? "legendSelect" : "legendUnSelect", name: series.name });
    }
    setIsolated(restore ? null : name);
  }
  const latencySeries = useMemo(() => [
    { name: data.js.avg, data: times.map((time, index) => [time * 1000, latency[index] ?? 0] as [number, number]), color: ChartPalette.semantic("Neutral", dark) },
  ], [times, latency, dark]);
  const richParts = data.hero.line2.split("{rich}");
  return <KumoPortalProvider container={overlayPortal}>
    <TooltipProvider>
    <div className="min-h-[100dvh] bg-kumo-base text-kumo-default">
      <div ref={topSentinel} aria-hidden="true" className="absolute top-0 left-0 h-px w-full" />
      <a className="fixed top-2 left-2 z-50 -translate-y-[160%] rounded-lg bg-kumo-contrast text-kumo-base px-3 py-2 focus-visible:translate-y-0" href="#main-content">{data.js.skipToContent}</a>
      <header className={`sticky top-0 z-40 -mb-12 transition-[background-color,box-shadow] duration-150 motion-reduce:transition-none ${scrolled
        ? "bg-[color-mix(in_srgb,var(--color-kumo-base)_60%,transparent)] shadow-[0_1px_12px_color-mix(in_srgb,var(--color-kumo-shadow-drop)_45%,transparent)] backdrop-blur-lg backdrop-saturate-[1.4]"
        : "bg-transparent"}`}>
        <div className="max-w-[1200px] mx-auto px-8 max-sm:px-4 min-h-12 flex items-center justify-between gap-4">
          <a className="font-semibold no-underline rounded-sm motion-safe:transition-colors motion-safe:duration-[120ms] hover:text-kumo-brand focus-visible:outline-2 focus-visible:outline-offset-[3px] focus-visible:outline-kumo-focus" href="/" translate="no">{data.brand}</a>
          <div className="flex items-center gap-2">
          <DropdownMenu>
            <Tooltip content={data.js.language} side="bottom" render={
              <DropdownMenu.Trigger render={<Button shape="square" variant="ghost" icon={Translate} aria-label={data.js.language} />} />
            } />
            <DropdownMenu.Content>
              {Object.entries(languages).map(([code, label]) => (
                <DropdownMenu.CheckboxItem key={code} translate="no" checked={code === data.lang.toLowerCase()}
                  onCheckedChange={() => { location.href = `/?hl=${code}`; }}>{label}</DropdownMenu.CheckboxItem>
              ))}
            </DropdownMenu.Content>
          </DropdownMenu>
          <Tooltip content={dark ? data.lightMode : data.darkMode} side="bottom" render={
            <Button shape="square" variant="ghost" aria-label={dark ? data.lightMode : data.darkMode}
              icon={dark ? Sun : Moon} onClick={() => setTheme(!dark)} />
          } />
          </div>
        </div>
      </header>

      <main id="main-content" className="max-w-[1200px] mx-auto px-8 max-sm:px-4 pb-18 max-sm:pb-14 flex flex-col gap-18 max-sm:gap-14">
        <section aria-labelledby="page-title"
          className="hero-wash relative isolate grid grid-cols-1 items-center justify-items-center gap-y-10 pt-[5.25rem] pb-12 max-sm:gap-y-8 lg:min-h-[min(43rem,calc(100dvh-1rem))] lg:grid-cols-[minmax(0,1fr)_minmax(0,24rem)] lg:justify-items-stretch lg:gap-y-0 lg:[column-gap:clamp(2.5rem,6vw,5.5rem)] lg:[padding-block:clamp(4.5rem,8vh,5.25rem)_clamp(2rem,4vh,2.5rem)]">
          <motion.div className="flex w-full min-w-0 flex-col items-center text-center lg:items-start lg:text-left"
            initial={reduceMotion ? false : "hidden"} animate="show"
            variants={{ hidden: {}, show: { transition: { staggerChildren: 0.09, delayChildren: 0.05 } } }}>
            <motion.h1 id="page-title"
              aria-label={`${data.hero.line1} ${data.hero.line2.replace("{rich}", data.hero.rich)}`}
              className="text-[clamp(2.5rem,3.8vw,3.5rem)] font-normal leading-[1.08] [word-break:keep-all] max-sm:text-[clamp(2.1rem,9.4vw,2.6rem)] max-sm:leading-[1.12]"
              variants={{ hidden: {}, show: { transition: { staggerChildren: 0.1 } } }}>
              <span className="-mb-[0.12em] block overflow-hidden pb-[0.12em]" aria-hidden="true">
                <motion.span className="block" variants={heroLine}>{data.hero.line1}</motion.span>
              </span>
              <span className="-mb-[0.12em] block overflow-hidden pb-[0.12em]" aria-hidden="true">
                <motion.span className="block" variants={heroLine}>
                  {richParts[0]}<em className="inline-block -skew-x-4 bg-[linear-gradient(90deg,var(--ig-orange)_0%,var(--ig-pink)_50%,var(--ig-lavender)_100%)] bg-clip-text pr-[0.06em] pl-[0.02em] font-semibold text-transparent not-italic sm:motion-safe:animate-rich-sweep">{data.hero.rich}</em>{richParts[1]}
                </motion.span>
              </span>
            </motion.h1>
            <motion.div className="mt-5 max-w-xl [&>*]:leading-[1.68]" variants={heroItem}><Text size="lg" variant="secondary">{data.tagline}</Text></motion.div>
            <motion.div className="mt-6 flex flex-wrap justify-center gap-3 lg:justify-start" variants={heroItem}>
              {data.supportURL ? <span className={`inline-flex ${lift}`}><LinkButton href={data.supportURL} external variant="primary" icon={Coffee}>{data.supportCta}</LinkButton></span> : null}
              {data.githubURL ? <span className={`inline-flex ${lift}`}><LinkButton href={data.githubURL} external variant="secondary" icon={GithubLogo} translate="no">GitHub</LinkButton></span> : null}
            </motion.div>
          </motion.div>
          <motion.div className="relative w-full min-w-0 max-lg:max-w-sm"
            initial={reduceMotion ? false : { opacity: 0, y: 18 }}
            animate={{ opacity: 1, y: 0 }} transition={{ duration: 0.6, delay: 0.25, ease: EASE }}>
            <div className="lg:motion-safe:animate-hero-float">
              <LivePreview brand={data.brand} host={data.host} reduceMotion={reduceMotion} />
            </div>
          </motion.div>
        </section>

        <Reveal reduceMotion={reduceMotion} className="flex flex-col gap-6 max-sm:gap-5" aria-labelledby="usage-title">
          <Text id="usage-title" variant="heading2" as="h2">{data.usageH2}</Text>
          <Grid variant="3up" gap="sm">
            <GridItem className="min-w-0"><div className={`h-full ${lift}`}><UsageCard title={data.normalView} url={<><span className="text-kumo-subtle">https://</span><HighlightedHost host={data.host} /></>} description={data.normalDesc} /></div></GridItem>
            <GridItem className="min-w-0"><div className={`h-full ${lift}`}><UsageCard title={data.galleryView} url={<><span className="text-kumo-subtle">https://</span><strong className="text-kumo-brand">g.</strong><HighlightedHost host={data.host} /></>} description={data.galleryDesc} /></div></GridItem>
            <GridItem className="min-w-0"><div className={`h-full ${lift}`}><UsageCard title={data.directView} url={<><span className="text-kumo-subtle">https://</span><strong className="text-kumo-brand">d.</strong><HighlightedHost host={data.host} /></>} description={data.directDesc} /></div></GridItem>
          </Grid>
        </Reveal>

        <Reveal reduceMotion={reduceMotion} className="flex flex-col gap-6 max-sm:gap-5" aria-labelledby="supported-title">
          <Text id="supported-title" variant="heading2" as="h2">{data.supportedH2}</Text>
          <Grid variant="3up" gap="sm">
            <GridItem className="min-w-0"><div className={`h-full ${lift}`}><LayerCard className="h-full"><LayerCard.Secondary>{data.posts}</LayerCard.Secondary><LayerCard.Primary className="grow"><Text variant="secondary">instagram.com/<strong className="text-kumo-default">p</strong>/…</Text><Text variant="secondary">instagram.com/username/<strong className="text-kumo-default">p</strong>/…</Text></LayerCard.Primary></LayerCard></div></GridItem>
            <GridItem className="min-w-0"><div className={`h-full ${lift}`}><LayerCard className="h-full"><LayerCard.Secondary>{data.reels}</LayerCard.Secondary><LayerCard.Primary className="grow"><Text variant="secondary">instagram.com/<strong className="text-kumo-default">reel</strong>(s)/…</Text><Text variant="secondary">instagram.com/username/<strong className="text-kumo-default">reel</strong>(s)/…</Text></LayerCard.Primary></LayerCard></div></GridItem>
            <GridItem className="min-w-0"><div className={`h-full ${lift}`}><LayerCard className="h-full"><LayerCard.Secondary>{data.userProfile}</LayerCard.Secondary><LayerCard.Primary className="grow"><Text variant="secondary">instagram.com/<strong className="text-kumo-default">username</strong></Text></LayerCard.Primary></LayerCard></div></GridItem>
          </Grid>
          <Banner icon={<Info weight="fill" />} title={data.supportNote} />
        </Reveal>

        <Reveal reduceMotion={reduceMotion} className="flex flex-col gap-6 max-sm:gap-5" aria-labelledby="status-title">
          <div className="flex flex-wrap items-center gap-3">
            <Text id="status-title" variant="heading2" as="h2">{data.statusH2}</Text>
            <div className="flex items-center gap-1.5 text-kumo-subtle"><Clock size={16} aria-hidden="true" /><Text variant="secondary" size="sm">{data.statusSub}</Text></div>
          </div>
          <div aria-live="polite">
          {statusFailed ? <Banner icon={<WarningCircle weight="fill" />} variant="error" title={data.js.statsUnavailable} /> : <Grid variant="2up" gap="base">
            <GridItem className="min-w-0"><LayerCard>
              <LayerCard.Secondary>{data.requests}</LayerCard.Secondary>
              <LayerCard.Primary>
                <div className="flex divide-x divide-kumo-line px-2 mb-2 overflow-x-auto">
                  <ChartLegend.LargeItem name={data.js.successful} color={successColor} value={format(sum(resolved))} className="shrink-0 not-first:pl-4"
                    inactive={isolated !== null && isolated !== data.js.successful} onClick={() => toggleSeries(data.js.successful)} />
                  <ChartLegend.LargeItem name={data.js.restricted} color={restrictedColor} value={format(sum(restricted))} className="shrink-0 not-first:pl-4"
                    inactive={isolated !== null && isolated !== data.js.restricted} onClick={() => toggleSeries(data.js.restricted)} />
                  <ChartLegend.LargeItem name={data.js.failed} color={errorColor} value={format(sum(failed))} className="shrink-0 not-first:pl-4"
                    inactive={isolated !== null && isolated !== data.js.failed} onClick={() => toggleSeries(data.js.failed)} />
                </div>
                {status !== null && !times.length ? <div className="min-h-[180px] grid place-items-center text-kumo-subtle text-[13px]">{data.js.noDataYet}</div> :
                  <TimeseriesChart ref={requestsChartRef} echarts={chartEcharts} isDarkMode={dark} data={requestSeries}
                    loading={status === null} height={300} xAxisName={chartTimeLabel} incomplete={incompleteAfter(times)}
                    enableLegendSelection ariaDescription={`${data.js.successful}, ${data.js.restricted}, ${data.js.failed}`} />}
              </LayerCard.Primary>
            </LayerCard></GridItem>
            <GridItem className="min-w-0"><LayerCard>
              <LayerCard.Secondary>{data.responseTime}</LayerCard.Secondary>
              <LayerCard.Primary>
                <div className="flex divide-x divide-kumo-line px-2 mb-2">
                  <ChartLegend.LargeItem name={data.js.avg} color={ChartPalette.semantic("Neutral", dark)} value={format(latest(latency))} unit={data.js.ms} />
                </div>
                <TimeseriesChart xAxisName={chartTimeLabel} echarts={chartEcharts} isDarkMode={dark} data={latencySeries} height={300} loading={status === null} incomplete={incompleteAfter(times)} ariaDescription={data.responseTime} />
              </LayerCard.Primary>
            </LayerCard></GridItem>
          </Grid>}
          </div>
        </Reveal>
      </main>

      <footer className="border-kumo-hairline border-t bg-kumo-recessed">
        <div className="max-w-[1200px] mx-auto px-8 max-sm:px-4 py-6 flex flex-col gap-2"><Text bold translate="no">{data.brand} ({data.version})</Text><Text variant="secondary" size="sm">{data.disclaimer}</Text></div>
      </footer>
      <div ref={overlayPortal} className="relative z-60" />
    </div>
    </TooltipProvider>
  </KumoPortalProvider>;
}

createRoot(document.getElementById("root")!).render(<App />);
