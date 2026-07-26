import React, { useEffect, useRef, useState } from "react";
import { Badge } from "@cloudflare/kumo/components/badge";
import { Banner } from "@cloudflare/kumo/components/banner";
import { Button, LinkButton } from "@cloudflare/kumo/components/button";
import { Grid, GridItem } from "@cloudflare/kumo/components/grid";
import { LayerCard } from "@cloudflare/kumo/components/layer-card";
import { DropdownMenu } from "@cloudflare/kumo/components/dropdown";
import { Text } from "@cloudflare/kumo/components/text";
import { Tooltip, TooltipProvider } from "@cloudflare/kumo/components/tooltip";
import { KumoPortalProvider } from "@cloudflare/kumo/utils";
import { ClockIcon as Clock } from "@phosphor-icons/react/Clock";
import { CoffeeIcon as Coffee } from "@phosphor-icons/react/Coffee";
import { GithubLogoIcon as GithubLogo } from "@phosphor-icons/react/GithubLogo";
import { InfoIcon as Info } from "@phosphor-icons/react/Info";
import { MoonIcon as Moon } from "@phosphor-icons/react/Moon";
import { SunIcon as Sun } from "@phosphor-icons/react/Sun";
import { TranslateIcon as Translate } from "@phosphor-icons/react/Translate";
import { WarningCircleIcon as WarningCircle } from "@phosphor-icons/react/WarningCircle";
import { LazyMotion, MotionConfig, domMin, useReducedMotion } from "motion/react";
import { canonicalServiceHost } from "../../shared/routes.ts";
import Preview, { HighlightedHost, type PreviewCopy } from "./preview.tsx";
import type { StatusCategory, StatusReport } from "./status.tsx";
import { Tabs } from "@cloudflare/kumo/components/tabs";

const StatusCharts = React.lazy(() => import("./status.tsx"));
type Copy = {
  successful: string; failed: string; restricted: string; ms: string;
  noDataYet: string; statsUnavailable: string; skipToContent: string; timeUTC: string; language: string;
};

type AppData = {
  brand: string; version: string; host: string; lang: string; tagline: string; hero: PreviewCopy; turnstileSiteKey: string;
  supportUrl: string; supportCta: string; githubUrl: string; darkMode: string; lightMode: string;
  usageH2: string; normalView: string; normalDesc: string; galleryView: string;
  galleryDesc: string; directView: string; directDesc: string; supportedH2: string;
  supportNote: string; posts: string; userProfile: string; reels: string; stories: string; all: string; beta: string;
  statusH2: string; statusSub: string; requests: string; responseTime: string;
  disclaimer: string; js: Copy;
};

const data = JSON.parse(document.getElementById("app-data")!.textContent!) as AppData;
const languages = { en: "English", es: "Español", fr: "Français", ja: "日本語", ko: "한국어", pt: "Português", "zh-hans": "简体中文", "zh-hant": "繁體中文" };

class ChartBoundary extends React.Component<React.PropsWithChildren, { failed: boolean }> {
  state = { failed: false };
  static getDerivedStateFromError() { return { failed: true }; }
  componentDidCatch(error: unknown) { console.error("status chart failed", error); }
  render() {
    return this.state.failed
      ? <Banner icon={<WarningCircle weight="fill" />} variant="error" title={data.js.statsUnavailable} />
      : this.props.children;
  }
}

function StatusChartsFallback() {
  return <Grid variant="2up" gap="base">
    {[data.requests, data.responseTime].map((title) => <GridItem key={title} className="min-w-0" aria-hidden="true">
      <LayerCard><LayerCard.Secondary>{title}</LayerCard.Secondary><LayerCard.Primary><div className="min-h-[340px]" /></LayerCard.Primary></LayerCard>
    </GridItem>)}
  </Grid>;
}

function Reveal({ children, className = "", ...props }: React.ComponentProps<"section">) {
  return <section data-reveal className={`reveal ${className}`} {...props}>{children}</section>;
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

function ThemeToggle({ dark, reduceMotion, onChange }: { dark: boolean; reduceMotion: boolean; onChange: (dark: boolean) => void }) {
  function setTheme(nextDark: boolean) {
    const apply = () => {
      document.documentElement.dataset.mode = nextDark ? "dark" : "light";
      localStorage.setItem("theme", nextDark ? "dark" : "light");
      onChange(nextDark);
    };
    if (reduceMotion || !document.startViewTransition) apply();
    else document.startViewTransition(apply);
  }

  return <Tooltip content={dark ? data.lightMode : data.darkMode} side="bottom" render={
    <Button shape="square" variant="ghost" aria-label={dark ? data.lightMode : data.darkMode}
      icon={dark ? Sun : Moon} onClick={() => setTheme(!dark)} />
  } />;
}

function App() {
  const serviceHost = canonicalServiceHost(data.host);
  const reduceMotion = useReducedMotion() ?? false;
  const [dark, setDark] = useState(() => document.documentElement.dataset.mode === "dark");
  const [scrolled, setScrolled] = useState(false);
  const topSentinel = useRef<HTMLDivElement>(null);
  const statusSection = useRef<HTMLElement>(null);
  const overlayPortal = useRef<HTMLDivElement>(null);
  const [status, setStatus] = useState<StatusReport | null>(null);
  const [statusTab, setStatusTab] = useState<StatusCategory>("all");
  const [statusFailed, setStatusFailed] = useState(false);
  const [showCharts, setShowCharts] = useState(false);
  useEffect(() => {
    if (!showCharts) return;
    const controller = new AbortController();
    fetch("/api/status", { signal: controller.signal }).then((response) => {
      if (!response.ok) throw new Error("status request failed");
      return response.json() as Promise<StatusReport>;
    }).then(setStatus).catch(() => {
      if (!controller.signal.aborted) setStatusFailed(true);
    });
    return () => controller.abort();
  }, [showCharts]);
  useEffect(() => {
    const el = topSentinel.current;
    if (!el) return;
    const io = new IntersectionObserver(([entry]) => setScrolled(!entry.isIntersecting));
    io.observe(el);
    return () => io.disconnect();
  }, []);
  useEffect(() => {
    const el = statusSection.current;
    if (!el) return;
    const io = new IntersectionObserver(([entry]) => {
      if (!entry.isIntersecting) return;
      setDark(document.documentElement.dataset.mode === "dark");
      setShowCharts(true);
      io.disconnect();
    }, { rootMargin: "600px 0px" });
    io.observe(el);
    return () => io.disconnect();
  }, []);
  useEffect(() => {
    if (reduceMotion) return;
    const elements = document.querySelectorAll<HTMLElement>("[data-reveal]");
    const io = new IntersectionObserver((entries) => {
      for (const entry of entries) {
        if (!entry.isIntersecting) continue;
        (entry.target as HTMLElement).classList.add("reveal-visible");
        io.unobserve(entry.target);
      }
    }, { threshold: 0.2 });
    elements.forEach((element) => io.observe(element));
    return () => io.disconnect();
  }, [reduceMotion]);
  const richParts = data.hero.line2.split("{rich}");
  return <KumoPortalProvider container={overlayPortal}>
    <TooltipProvider>
    <div className="min-h-[100dvh] bg-kumo-base text-kumo-default">
      <div ref={topSentinel} aria-hidden="true" className="absolute top-0 left-0 h-px w-full" />
      <a className="fixed top-2 left-2 z-50 -translate-y-[160%] rounded-lg bg-kumo-contrast text-kumo-base px-3 py-2 focus-visible:translate-y-0" href="#main-content">{data.js.skipToContent}</a>
      <header className="sticky top-0 z-40 -mb-12">
        <div aria-hidden="true"
          className={`header-surface pointer-events-none absolute inset-0 bg-[color-mix(in_srgb,var(--color-kumo-base)_60%,transparent)] shadow-[0_1px_12px_color-mix(in_srgb,var(--color-kumo-shadow-drop)_45%,transparent)] backdrop-blur-lg backdrop-saturate-[1.4] ${scrolled ? "header-surface-visible" : ""}`} />
        <div className="relative max-w-[1200px] mx-auto px-8 max-sm:px-4 min-h-12 flex items-center justify-between gap-4">
          <a className="brand-link font-semibold no-underline rounded-sm focus-visible:outline-2 focus-visible:outline-offset-[3px] focus-visible:outline-kumo-focus"
            href="/" translate="no">{data.brand}</a>
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
          <ThemeToggle dark={dark} reduceMotion={reduceMotion} onChange={setDark} />
          </div>
        </div>
      </header>

      <main id="main-content" className="max-w-[1200px] mx-auto px-8 max-sm:px-4 pb-18 max-sm:pb-14 flex flex-col gap-18 max-sm:gap-14">
        <section aria-labelledby="page-title"
          className="hero-wash relative isolate grid grid-cols-1 items-center justify-items-center gap-y-10 pt-[5.25rem] pb-12 max-sm:gap-y-8 lg:min-h-[min(43rem,calc(100dvh-1rem))] lg:grid-cols-[minmax(0,1fr)_minmax(0,24rem)] lg:justify-items-stretch lg:gap-y-0 lg:[column-gap:clamp(2.5rem,6vw,5.5rem)] lg:[padding-block:clamp(4.5rem,8vh,5.25rem)_clamp(2rem,4vh,2.5rem)]">
          <div className="flex w-full min-w-0 flex-col items-center text-center lg:items-start lg:text-left">
            <h1 id="page-title"
              aria-label={`${data.hero.line1} ${data.hero.line2.replace("{rich}", data.hero.rich)}`}
              className="text-[clamp(2.5rem,3.8vw,3.5rem)] font-normal leading-[1.08] [word-break:keep-all] max-sm:text-[clamp(2.1rem,9.4vw,2.6rem)] max-sm:leading-[1.12]">
              <span className="-mb-[0.12em] block overflow-hidden pb-[0.12em]" aria-hidden="true">
                <span className="hero-line hero-line-first block">{data.hero.line1}</span>
              </span>
              <span className="-mb-[0.12em] block overflow-hidden pb-[0.12em]" aria-hidden="true">
                <span className="hero-line hero-line-second block">
                  {richParts[0]}<em
                    className="hero-rich-motion inline-block -skew-x-4 bg-[linear-gradient(90deg,var(--ig-orange)_0%,var(--ig-pink)_50%,var(--ig-lavender)_100%)] bg-clip-text pr-[0.06em] pl-[0.02em] font-semibold text-transparent not-italic">{data.hero.rich}</em>{richParts[1]}
                </span>
              </span>
            </h1>
            <div className="hero-item hero-copy mt-5 max-w-xl [&>*]:leading-[1.68]"><Text size="lg" variant="secondary">{data.tagline}</Text></div>
            <div className="hero-item hero-actions mt-6 flex flex-wrap justify-center gap-3 lg:justify-start">
              {data.supportUrl ? <span className="motion-lift inline-flex"><LinkButton href={data.supportUrl} external variant="primary" icon={Coffee}>{data.supportCta}</LinkButton></span> : null}
              {data.githubUrl ? <span className="motion-lift inline-flex"><LinkButton href={data.githubUrl} external variant="secondary" icon={GithubLogo} translate="no">GitHub</LinkButton></span> : null}
            </div>
          </div>
          <div className="hero-preview relative w-full min-w-0 max-lg:max-w-sm">
            <Preview brand={data.brand} host={serviceHost} lang={data.lang} copy={data.hero} turnstileSiteKey={data.turnstileSiteKey}
              reduceMotion={reduceMotion} />
          </div>
        </section>

        <Reveal className="flex flex-col gap-6 max-sm:gap-5" aria-labelledby="usage-title">
          <Text id="usage-title" variant="heading2" as="h2">{data.usageH2}</Text>
          <Grid variant="3up" gap="sm">
            <GridItem className="min-w-0"><div className="motion-lift h-full"><UsageCard title={data.normalView} url={<><span className="text-kumo-subtle">https://</span><HighlightedHost host={serviceHost} /></>} description={data.normalDesc} /></div></GridItem>
            <GridItem className="min-w-0"><div className="motion-lift h-full"><UsageCard title={data.galleryView} url={<><span className="text-kumo-subtle">https://</span><strong className="text-kumo-brand">g.</strong><HighlightedHost host={serviceHost} /></>} description={data.galleryDesc} /></div></GridItem>
            <GridItem className="min-w-0"><div className="motion-lift h-full"><UsageCard title={data.directView} url={<><span className="text-kumo-subtle">https://</span><strong className="text-kumo-brand">d.</strong><HighlightedHost host={serviceHost} /></>} description={data.directDesc} /></div></GridItem>
          </Grid>
        </Reveal>

        <Reveal className="flex flex-col gap-6 max-sm:gap-5" aria-labelledby="supported-title">
          <Text id="supported-title" variant="heading2" as="h2">{data.supportedH2}</Text>
          <Grid variant="2up" gap="sm">
            <GridItem className="min-w-0"><div className="motion-lift h-full"><LayerCard className="h-full"><LayerCard.Secondary>{data.posts}</LayerCard.Secondary><LayerCard.Primary className="grow"><Text variant="secondary">instagram.com/<strong className="text-kumo-default">p</strong>/…</Text><Text variant="secondary">instagram.com/username/<strong className="text-kumo-default">p</strong>/…</Text></LayerCard.Primary></LayerCard></div></GridItem>
            <GridItem className="min-w-0"><div className="motion-lift h-full"><LayerCard className="h-full"><LayerCard.Secondary>{data.reels}</LayerCard.Secondary><LayerCard.Primary className="grow"><Text variant="secondary">instagram.com/<strong className="text-kumo-default">reel</strong>(s)/…</Text><Text variant="secondary">instagram.com/username/<strong className="text-kumo-default">reel</strong>(s)/…</Text></LayerCard.Primary></LayerCard></div></GridItem>
            <GridItem className="min-w-0"><div className="motion-lift h-full"><LayerCard className="h-full"><LayerCard.Secondary><span className="flex items-center gap-2">{data.stories}<Badge variant="beta">{data.beta}</Badge></span></LayerCard.Secondary><LayerCard.Primary className="grow"><Text variant="secondary">instagram.com/<strong className="text-kumo-default">stories</strong>/username/…</Text></LayerCard.Primary></LayerCard></div></GridItem>
            <GridItem className="min-w-0"><div className="motion-lift h-full"><LayerCard className="h-full"><LayerCard.Secondary>{data.userProfile}</LayerCard.Secondary><LayerCard.Primary className="grow"><Text variant="secondary">instagram.com/<strong className="text-kumo-default">username</strong></Text></LayerCard.Primary></LayerCard></div></GridItem>
          </Grid>
          <Banner icon={<Info weight="fill" />} title={data.supportNote} />
        </Reveal>

        <Reveal ref={statusSection} className="flex flex-col gap-6 max-sm:gap-5" aria-labelledby="status-title">
          <div className="flex flex-wrap items-center gap-3">
            <Text id="status-title" variant="heading2" as="h2">{data.statusH2}</Text>
            <div className="flex items-center gap-1.5 text-kumo-subtle"><Clock size={16} aria-hidden="true" /><Text variant="secondary" size="sm">{data.statusSub}</Text></div>
          </div>
          {statusFailed ? null : <Tabs className="self-start" value={statusTab} onValueChange={(value) => setStatusTab(value as StatusCategory)}
            tabs={[
              { value: "all", label: data.all },
              { value: "posts", label: `${data.posts}/${data.reels}` },
              { value: "stories", label: data.stories },
              { value: "profile", label: data.userProfile },
            ]} />}
          <div aria-live="polite">
          {statusFailed ? <Banner icon={<WarningCircle weight="fill" />} variant="error" title={data.js.statsUnavailable} /> : showCharts
            ? <ChartBoundary><React.Suspense fallback={<StatusChartsFallback />}>
                <StatusCharts status={status ? status[statusTab] : null} dark={dark} lang={data.lang} requests={data.requests} responseTime={data.responseTime} copy={data.js} />
              </React.Suspense></ChartBoundary>
            : <StatusChartsFallback />}
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

export function Root() {
  return <LazyMotion features={domMin} strict><MotionConfig reducedMotion="user"><App /></MotionConfig></LazyMotion>;
}
