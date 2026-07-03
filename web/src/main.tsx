import React, { useEffect, useMemo, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import { Banner } from "@cloudflare/kumo/components/banner";
import { Button, LinkButton } from "@cloudflare/kumo/components/button";
import { Grid } from "@cloudflare/kumo/components/grid";
import { LayerCard } from "@cloudflare/kumo/components/layer-card";
import { Select } from "@cloudflare/kumo/components/select";
import { Text } from "@cloudflare/kumo/components/text";
import { Tooltip, TooltipProvider } from "@cloudflare/kumo/components/tooltip";
import { ChartLegend, ChartPalette, TimeseriesChart } from "@cloudflare/kumo/components/chart";
import "@cloudflare/kumo/styles/standalone";
import { Clock, Coffee, GithubLogo, Info, Moon, Sun } from "@phosphor-icons/react";
import * as echarts from "echarts/core";
import { LineChart } from "echarts/charts";
import { AriaComponent, AxisPointerComponent, BrushComponent, GridComponent, LegendComponent, ToolboxComponent, TooltipComponent } from "echarts/components";
import { CanvasRenderer } from "echarts/renderers";
import { LabelLayout, UniversalTransition } from "echarts/features";
import "./styles.css";

echarts.use([LineChart, AxisPointerComponent, BrushComponent, GridComponent, LegendComponent, ToolboxComponent, TooltipComponent, AriaComponent, LabelLayout, UniversalTransition, CanvasRenderer]);

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
  successful: string; failed: string; avg: string; ms: string; noData: string;
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

type Status = { t?: number[]; resolved?: number[]; failed?: number[]; latency?: number[] };

const data = JSON.parse(document.getElementById("app-data")!.textContent!) as AppData;
const languages = { en: "English", ja: "日本語", ko: "한국어" };
const EMPTY_VALUES: number[] = [];
const browserTimeZone = Intl.DateTimeFormat().resolvedOptions().timeZone || "Local";
const chartTimeLabel = data.js.timeUTC.replace("UTC", browserTimeZone);

function sum(values: number[] = []) { return values.reduce((total, value) => total + value, 0); }
function latest(values: number[] = []) { return values.length ? values[values.length - 1] : 0; }
const numberFormatter = new Intl.NumberFormat(data.lang, { maximumFractionDigits: 0 });
function format(value: number) { return numberFormatter.format(Math.round(value)); }

function HighlightedHost({ host }: { host: string }) {
  if (!host.toLowerCase().startsWith("og")) return host;
  return <><strong className="key-prefix">{host.slice(0, 2)}</strong>{host.slice(2)}</>;
}

// The last 10-minute bucket is still being aggregated; dash it as incomplete.
function incompleteAfter(times: number[]): { after: number } | undefined {
  return times.length >= 2 ? { after: times[times.length - 2] * 1000 } : undefined;
}

function UsageCard({ title, url, description }: { title: string; url: React.ReactNode; description: string }) {
  return <LayerCard className="min-w-0">
    <LayerCard.Secondary>{title}</LayerCard.Secondary>
    <LayerCard.Primary>
      <div className="flex flex-col gap-3">
        <div className="break-words"><Text variant="body" size="base">{url}</Text></div>
        <Text variant="secondary" size="base">{description}</Text>
      </div>
    </LayerCard.Primary>
  </LayerCard>;
}

function App() {
  const [dark, setDark] = useState(document.documentElement.dataset.mode === "dark");
  const [scrolled, setScrolled] = useState(window.scrollY > 8);
  const [overlayPortal, setOverlayPortal] = useState<HTMLDivElement | null>(null);
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
  const failed = status?.failed ?? EMPTY_VALUES;
  const latency = status?.latency ?? EMPTY_VALUES;
  const successColor = ChartPalette.semantic("Success", dark);
  const errorColor = ChartPalette.semantic("Attention", dark);
  const requestSeries = useMemo(() => [
    { name: data.js.successful, color: successColor, data: times.map((time, index) => [time * 1000, resolved[index] ?? 0] as [number, number]) },
    { name: data.js.failed, color: errorColor, data: times.map((time, index) => [time * 1000, failed[index] ?? 0] as [number, number]) },
  ], [times, resolved, failed, successColor, errorColor]);
  const requestsChartRef = useRef<echarts.ECharts>(null);
  // Name of the one hidden series, or null when both are visible.
  const [hidden, setHidden] = useState<string | null>(null);
  // A theme switch re-inits the ECharts instance, resetting legend selection
  // to all-visible; reset our state to match so the legend doesn't desync.
  useEffect(() => { setHidden(null); }, [dark]);
  // Click isolates a series via the hidden ECharts legend; clicking the
  // already-isolated series restores both.
  function toggleSeries(name: string) {
    const chart = requestsChartRef.current;
    if (!chart) return;
    const other = name === data.js.successful ? data.js.failed : data.js.successful;
    const restore = hidden === other;
    chart.dispatchAction({ type: "legendSelect", name });
    chart.dispatchAction({ type: restore ? "legendSelect" : "legendUnSelect", name: other });
    setHidden(restore ? null : other);
  }
  const latencySeries = useMemo(() => [
    { name: data.js.avg, data: times.map((time, index) => [time * 1000, latency[index] ?? 0] as [number, number]), color: ChartPalette.semantic("Neutral", dark) },
  ], [times, latency, dark]);
  return <TooltipProvider>
    <div className="min-h-screen bg-kumo-base text-kumo-default">
      <a className="skip-link" href="#main-content">{data.js.skipToContent}</a>
      <header className={`site-header ${scrolled ? "is-scrolled" : "is-top"}`}>
        <div className="page-shell gnb-inner flex items-center justify-between gap-4">
          <a className="site-brand font-semibold no-underline" href="/" translate="no">{data.brand}</a>
          <div className="flex items-center gap-2">
          <Select
            aria-label={data.js.language}
            size="base"
            className="w-32"
            container={overlayPortal}
            value={data.lang}
            items={languages}
            onValueChange={(value) => { if (value) location.href = `/${value}`; }}
          />
          <Tooltip container={overlayPortal} content={dark ? data.lightMode : data.darkMode} side="bottom" render={
            <Button shape="square" size="base" variant="ghost" aria-label={dark ? data.lightMode : data.darkMode}
              icon={dark ? Sun : Moon} onClick={() => setTheme(!dark)} />
          } />
          </div>
        </div>
      </header>

      <main id="main-content" className="page-main">
        <section className="hero" aria-labelledby="page-title">
          <div className="hero-copy">
            <Text id="page-title" variant="heading1" as="h1">{data.brand}</Text>
            <div className="max-w-3xl text-pretty"><Text size="lg" variant="secondary">{data.tagline}</Text></div>
            <div className="flex flex-wrap gap-3">
              {data.supportURL ? <LinkButton className="cta-gradient" href={data.supportURL} external variant="primary" icon={Coffee}>{data.supportCta}</LinkButton> : null}
              {data.githubURL ? <LinkButton href={data.githubURL} external variant="secondary" icon={GithubLogo} translate="no">GitHub</LinkButton> : null}
            </div>
          </div>
        </section>

        <section className="page-section" aria-labelledby="usage-title">
          <Text id="usage-title" variant="heading2" as="h2">{data.usageH2}</Text>
          <Grid className="card-grid" variant="3up" gap="sm">
            <UsageCard title={data.normalView} url={<><span className="text-kumo-subtle">https://</span><HighlightedHost host={data.host} /></>} description={data.normalDesc} />
            <UsageCard title={data.galleryView} url={<><span className="text-kumo-subtle">https://</span><strong className="key-prefix">g.</strong><HighlightedHost host={data.host} /></>} description={data.galleryDesc} />
            <UsageCard title={data.directView} url={<><span className="text-kumo-subtle">https://</span><strong className="key-prefix">d.</strong><HighlightedHost host={data.host} /></>} description={data.directDesc} />
          </Grid>
        </section>

        <section className="page-section" aria-labelledby="supported-title">
          <Text id="supported-title" variant="heading2" as="h2">{data.supportedH2}</Text>
          <Grid variant="3up" gap="sm" className="card-grid">
            <LayerCard><LayerCard.Secondary>{data.posts}</LayerCard.Secondary><LayerCard.Primary><Text variant="secondary" size="base">instagram.com/<strong className="text-kumo-default">p</strong>/…</Text><Text variant="secondary" size="base">instagram.com/username/<strong className="text-kumo-default">p</strong>/…</Text></LayerCard.Primary></LayerCard>
            <LayerCard><LayerCard.Secondary>{data.reels}</LayerCard.Secondary><LayerCard.Primary><Text variant="secondary" size="base">instagram.com/<strong className="text-kumo-default">reel</strong>(s)/…</Text><Text variant="secondary" size="base">instagram.com/username/<strong className="text-kumo-default">reel</strong>(s)/…</Text></LayerCard.Primary></LayerCard>
            <LayerCard><LayerCard.Secondary>{data.userProfile}</LayerCard.Secondary><LayerCard.Primary><Text variant="secondary" size="base">instagram.com/<strong className="text-kumo-default">username</strong></Text></LayerCard.Primary></LayerCard>
          </Grid>
          <Banner icon={<Info weight="fill" aria-hidden="true" />} title={data.supportNote} />
        </section>

        <section className="page-section" aria-labelledby="status-title">
          <div className="flex flex-wrap items-center gap-3">
            <Text id="status-title" variant="heading2" as="h2">{data.statusH2}</Text>
            <div className="flex items-center gap-1.5 text-kumo-subtle"><Clock size={16} aria-hidden="true" /><Text variant="secondary" size="sm">{data.statusSub}</Text></div>
          </div>
          <div aria-live="polite">
          {statusFailed ? <Banner variant="error" title={data.js.statsUnavailable} /> : <div className="status-stack">
            <LayerCard className="min-w-0">
              <LayerCard.Secondary>{data.requests}</LayerCard.Secondary>
              <LayerCard.Primary>
                <div className="status-legend flex divide-x divide-kumo-hairline gap-4 px-2 mb-2">
                  <ChartLegend.LargeItem name={data.js.successful} color={successColor} value={format(sum(resolved))}
                    inactive={hidden === data.js.successful} onClick={() => toggleSeries(data.js.successful)} />
                  <ChartLegend.LargeItem name={data.js.failed} color={errorColor} value={format(sum(failed))}
                    inactive={hidden === data.js.failed} onClick={() => toggleSeries(data.js.failed)} />
                </div>
                {status !== null && !times.length ? <div className="chart-empty">{data.js.noDataYet}</div> :
                  <TimeseriesChart ref={requestsChartRef} echarts={chartEcharts} isDarkMode={dark} data={requestSeries}
                    loading={status === null} height={300} xAxisName={chartTimeLabel} incomplete={incompleteAfter(times)}
                    enableLegendSelection ariaDescription={`${data.js.successful}, ${data.js.failed}`} />}
              </LayerCard.Primary>
            </LayerCard>
            <LayerCard>
              <LayerCard.Secondary>{data.responseTime}</LayerCard.Secondary>
              <LayerCard.Primary>
                <div className="status-legend flex divide-x divide-kumo-hairline gap-4 px-2 mb-2">
                  <ChartLegend.LargeItem name={data.js.avg} color={ChartPalette.semantic("Neutral", dark)} value={format(latest(latency))} unit={data.js.ms} />
                </div>
                <TimeseriesChart xAxisName={chartTimeLabel} echarts={chartEcharts} isDarkMode={dark} data={latencySeries} height={300} loading={status === null} incomplete={incompleteAfter(times)} ariaDescription={data.responseTime} />
              </LayerCard.Primary>
            </LayerCard>
          </div>}
          </div>
        </section>
      </main>

      <footer className="border-kumo-hairline border-t bg-kumo-recessed">
        <div className="page-shell footer-inner flex flex-col gap-2"><Text bold translate="no">{data.brand} ({data.version})</Text><Text variant="secondary" size="sm">{data.disclaimer}</Text></div>
      </footer>
      <div ref={setOverlayPortal} className="overlay-portal" />
    </div>
  </TooltipProvider>;
}

createRoot(document.getElementById("root")!).render(<App />);
