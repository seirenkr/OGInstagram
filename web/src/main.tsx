import React, { useEffect, useMemo, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import { Banner } from "@cloudflare/kumo/components/banner";
import { Button, LinkButton } from "@cloudflare/kumo/components/button";
import { Grid, GridItem } from "@cloudflare/kumo/components/grid";
import { LayerCard } from "@cloudflare/kumo/components/layer-card";
import { Select } from "@cloudflare/kumo/components/select";
import { Text } from "@cloudflare/kumo/components/text";
import { Tooltip, TooltipProvider } from "@cloudflare/kumo/components/tooltip";
import { KumoPortalProvider } from "@cloudflare/kumo/utils";
import { ChartLegend, ChartPalette, TimeseriesChart } from "@cloudflare/kumo/components/chart";
import { Clock, Coffee, GithubLogo, Info, Moon, Sun, WarningCircle } from "@phosphor-icons/react";
import * as echarts from "echarts/core";
import { LineChart } from "echarts/charts";
import { AriaComponent, AxisPointerComponent, BrushComponent, GridComponent, LegendComponent, ToolboxComponent, TooltipComponent } from "echarts/components";
import { CanvasRenderer } from "echarts/renderers";
import "./styles.css";

echarts.use([LineChart, AxisPointerComponent, BrushComponent, GridComponent, LegendComponent, ToolboxComponent, TooltipComponent, AriaComponent, CanvasRenderer]);

const CHART_FONT_FAMILY = '"Pretendard JP Variable", Pretendard, ui-sans-serif, system-ui, sans-serif';
const chartEcharts = {
  ...echarts,
  init: (...args: Parameters<typeof echarts.init>) => {
    const chart = echarts.init(...args);
    chart.setOption({ useUTC: false, textStyle: { fontFamily: CHART_FONT_FAMILY } });
    return chart;
  },
} as typeof echarts;

type Copy = {
  successful: string; failed: string; restricted: string; avg: string; ms: string; noData: string;
  noDataYet: string; statsUnavailable: string; skipToContent: string; timeUTC: string; language: string;
};

type AppData = {
  brand: string; version: string; host: string; lang: string; tagline: string;
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

function HighlightedHost({ host }: { host: string }) {
  if (!host.toLowerCase().startsWith("og")) return host;
  return <><strong className="text-kumo-brand">{host.slice(0, 2)}</strong>{host.slice(2)}</>;
}

// The last 10-minute bucket is still being aggregated; dash it as incomplete.
function incompleteAfter(times: number[]): { after: number } | undefined {
  return times.length >= 2 ? { after: times[times.length - 2] * 1000 } : undefined;
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
  const [dark, setDark] = useState(document.documentElement.dataset.mode === "dark");
  const [scrolled, setScrolled] = useState(window.scrollY > 8);
  const overlayPortal = useRef<HTMLDivElement>(null);
  const [status, setStatus] = useState<Status | null>(null);
  const [statusFailed, setStatusFailed] = useState(false);
  useEffect(() => {
    fetch("/_status").then((response) => {
      if (!response.ok) throw new Error("status request failed");
      return response.json() as Promise<Status>;
    }).then(setStatus).catch(() => setStatusFailed(true));
  }, []);
  useEffect(() => {
    const onScroll = () => setScrolled(window.scrollY > 8);
    window.addEventListener("scroll", onScroll, { passive: true });
    return () => window.removeEventListener("scroll", onScroll);
  }, []);
  function setTheme(nextDark: boolean) {
    document.documentElement.dataset.mode = nextDark ? "dark" : "light";
    localStorage.setItem("theme", nextDark ? "dark" : "light");
    setDark(nextDark);
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
  return <KumoPortalProvider container={overlayPortal}>
    <TooltipProvider>
    <div className="min-h-screen bg-kumo-base text-kumo-default">
      <a className="fixed top-2 left-2 z-50 -translate-y-[160%] rounded-lg bg-kumo-contrast text-kumo-base px-3 py-2 focus-visible:translate-y-0" href="#main-content">{data.js.skipToContent}</a>
      <header className={`site-header ${scrolled ? "is-scrolled" : "is-top"}`}>
        <div className="max-w-[1200px] mx-auto px-8 max-sm:px-4 min-h-14 flex items-center justify-between gap-4">
          <a className="font-semibold no-underline rounded-sm motion-safe:transition-colors motion-safe:duration-[120ms] hover:text-kumo-brand focus-visible:outline-2 focus-visible:outline-offset-[3px] focus-visible:outline-kumo-focus" href="/" translate="no">{data.brand}</a>
          <div className="flex items-center gap-2">
          <Select
            aria-label={data.js.language}
            className="w-32"
            value={data.lang.toLowerCase()}
            items={languages}
            onValueChange={(value) => { if (value) location.href = `/?hl=${value}`; }}
          />
          <Tooltip content={dark ? data.lightMode : data.darkMode} side="bottom" render={
            <Button shape="square" variant="ghost" aria-label={dark ? data.lightMode : data.darkMode}
              icon={dark ? Sun : Moon} onClick={() => setTheme(!dark)} />
          } />
          </div>
        </div>
      </header>

      <main id="main-content" className="max-w-[1200px] mx-auto px-8 max-sm:px-4 pb-18 max-sm:pb-14 flex flex-col gap-18 max-sm:gap-14">
        <section className="hero relative min-h-[400px] max-sm:min-h-[340px] flex items-center py-16 max-sm:py-12" aria-labelledby="page-title">
          <div className="relative z-[1] max-w-[760px] flex flex-col items-start gap-5">
            <Text id="page-title" variant="heading1" as="h1">{data.brand}</Text>
            <div className="max-w-3xl text-pretty"><Text size="lg" variant="secondary">{data.tagline}</Text></div>
            <div className="flex flex-wrap gap-3">
              {data.supportURL ? <LinkButton href={data.supportURL} external variant="primary" icon={Coffee}>{data.supportCta}</LinkButton> : null}
              {data.githubURL ? <LinkButton href={data.githubURL} external variant="secondary" icon={GithubLogo} translate="no">GitHub</LinkButton> : null}
            </div>
          </div>
        </section>

        <section className="flex flex-col gap-6 max-sm:gap-5" aria-labelledby="usage-title">
          <Text id="usage-title" variant="heading2" as="h2">{data.usageH2}</Text>
          <Grid variant="3up" gap="sm">
            <GridItem className="min-w-0"><UsageCard title={data.normalView} url={<><span className="text-kumo-subtle">https://</span><HighlightedHost host={data.host} /></>} description={data.normalDesc} /></GridItem>
            <GridItem className="min-w-0"><UsageCard title={data.galleryView} url={<><span className="text-kumo-subtle">https://</span><strong className="text-kumo-brand">g.</strong><HighlightedHost host={data.host} /></>} description={data.galleryDesc} /></GridItem>
            <GridItem className="min-w-0"><UsageCard title={data.directView} url={<><span className="text-kumo-subtle">https://</span><strong className="text-kumo-brand">d.</strong><HighlightedHost host={data.host} /></>} description={data.directDesc} /></GridItem>
          </Grid>
        </section>

        <section className="flex flex-col gap-6 max-sm:gap-5" aria-labelledby="supported-title">
          <Text id="supported-title" variant="heading2" as="h2">{data.supportedH2}</Text>
          <Grid variant="3up" gap="sm">
            <GridItem className="min-w-0"><LayerCard className="h-full"><LayerCard.Secondary>{data.posts}</LayerCard.Secondary><LayerCard.Primary className="grow"><Text variant="secondary">instagram.com/<strong className="text-kumo-default">p</strong>/…</Text><Text variant="secondary">instagram.com/username/<strong className="text-kumo-default">p</strong>/…</Text></LayerCard.Primary></LayerCard></GridItem>
            <GridItem className="min-w-0"><LayerCard className="h-full"><LayerCard.Secondary>{data.reels}</LayerCard.Secondary><LayerCard.Primary className="grow"><Text variant="secondary">instagram.com/<strong className="text-kumo-default">reel</strong>(s)/…</Text><Text variant="secondary">instagram.com/username/<strong className="text-kumo-default">reel</strong>(s)/…</Text></LayerCard.Primary></LayerCard></GridItem>
            <GridItem className="min-w-0"><LayerCard className="h-full"><LayerCard.Secondary>{data.userProfile}</LayerCard.Secondary><LayerCard.Primary className="grow"><Text variant="secondary">instagram.com/<strong className="text-kumo-default">username</strong></Text></LayerCard.Primary></LayerCard></GridItem>
          </Grid>
          <Banner icon={<Info weight="fill" />} title={data.supportNote} />
        </section>

        <section className="flex flex-col gap-6 max-sm:gap-5" aria-labelledby="status-title">
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
        </section>
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
