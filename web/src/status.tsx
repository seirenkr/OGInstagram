import { useMemo, useRef, useState } from "react";
import { ChartLegend, ChartPalette, TimeseriesChart } from "@cloudflare/kumo/components/chart";
import { Grid, GridItem } from "@cloudflare/kumo/components/grid";
import { LayerCard } from "@cloudflare/kumo/components/layer-card";
import * as echarts from "echarts/core";
import { LineChart } from "echarts/charts";
import { AriaComponent, AxisPointerComponent, GridComponent, LegendComponent, TooltipComponent } from "echarts/components";
import { SVGRenderer } from "echarts/renderers";
import type en from "../locales/en.json";

echarts.use([LineChart, AxisPointerComponent, GridComponent, LegendComponent, TooltipComponent, AriaComponent, SVGRenderer]);

const chartEcharts = {
  ...echarts,
  init: (dom: Parameters<typeof echarts.init>[0], theme?: Parameters<typeof echarts.init>[1], options?: Parameters<typeof echarts.init>[2]) => {
    const chart = echarts.init(dom, theme, { ...options, renderer: "svg" });
    const fontFamily = getComputedStyle(document.documentElement).getPropertyValue("--font-sans").trim();
    chart.setOption({ textStyle: { fontFamily } });
    return chart;
  },
} as typeof echarts;

export type Status = {
  t?: number[];
  resolved?: number[];
  restricted?: number[];
  failed?: number[];
  p50?: number[];
  p90?: number[];
  p99?: number[];
};

export type StatusCategory = "all" | "posts" | "stories" | "profile";
export type StatusReport = Record<StatusCategory, Status>;

type StatusChartsProps = {
  status: Status | null;
  dark: boolean;
  lang: string;
  requests: string;
  responseTime: string;
  copy: typeof en.js;
};

type Series = { name: string; color: string; values: number[] };

const EMPTY_VALUES: number[] = [];

function sum(values: number[]) { return values.reduce((total, value) => total + value, 0); }
function latest(values: number[]) { return values.length ? values[values.length - 1] : 0; }

function useSeriesIsolation(dark: boolean, series: { name: string }[]) {
  const chartRef = useRef<echarts.ECharts>(null);
  const [selection, setSelection] = useState<{ dark: boolean; name: string | null }>({ dark, name: null });
  const isolated = selection.dark === dark ? selection.name : null;
  function toggle(name: string) {
    const chart = chartRef.current;
    if (!chart) return;
    const restore = isolated === name;
    for (const item of series) {
      chart.dispatchAction({ type: restore || item.name === name ? "legendSelect" : "legendUnSelect", name: item.name });
    }
    setSelection({ dark, name: restore ? null : name });
  }
  return { chartRef, isolated, toggle };
}

function SeriesCard({ title, series, legendValue, ariaDescription, times, loading, dark, xAxisName, noDataYet }: {
  title: string;
  series: Series[];
  legendValue: (values: number[]) => string;
  ariaDescription: string;
  times: number[];
  loading: boolean;
  dark: boolean;
  xAxisName: string;
  noDataYet: string;
}) {
  const data = useMemo(() => series.map(({ name, color, values }) => ({
    name, color, data: times.map((time, index) => [time * 1000, values[index] ?? 0] as [number, number]),
  })), [series, times]);
  const { chartRef, isolated, toggle } = useSeriesIsolation(dark, series);
  return <GridItem className="min-w-0"><LayerCard>
    <LayerCard.Secondary>{title}</LayerCard.Secondary>
    <LayerCard.Primary>
      <div className="flex flex-wrap gap-4">
        {series.map(({ name, color, values }) => <ChartLegend.SmallItem key={name} name={name} color={color} value={legendValue(values)}
          inactive={isolated !== null && isolated !== name} onClick={() => toggle(name)} />)}
      </div>
      {!loading && !times.length ? <div className="min-h-[300px] grid place-items-center text-kumo-subtle text-[13px]">{noDataYet}</div> :
        <TimeseriesChart ref={chartRef} echarts={chartEcharts} isDarkMode={dark} data={data}
          height={300} xAxisName={xAxisName} incomplete={times.length >= 2 ? { after: times[times.length - 2] * 1000 } : undefined}
          loading={loading} enableLegendSelection ariaDescription={ariaDescription} />}
    </LayerCard.Primary>
  </LayerCard></GridItem>;
}

export default function StatusCharts({ status, dark, lang, requests, responseTime, copy }: StatusChartsProps) {
  const numberFormatter = useMemo(() => new Intl.NumberFormat(lang, { maximumFractionDigits: 0 }), [lang]);
  const format = (value: number) => numberFormatter.format(Math.round(value));
  const chartTimeLabel = copy.timeUTC.replace("UTC",
    new Intl.DateTimeFormat(undefined, { timeZoneName: "shortOffset" })
      .formatToParts().find((part) => part.type === "timeZoneName")?.value || "UTC");
  const requestSeries = useMemo(() => [
    { name: copy.successful, color: ChartPalette.semantic("Success", dark), values: status?.resolved ?? EMPTY_VALUES },
    { name: copy.restricted, color: ChartPalette.semantic("Warning", dark), values: status?.restricted ?? EMPTY_VALUES },
    { name: copy.failed, color: ChartPalette.semantic("Attention", dark), values: status?.failed ?? EMPTY_VALUES },
  ], [status, dark, copy]);
  const percentileSeries = useMemo(() => (["p50", "p90", "p99"] as const).map((key, index) => ({
    name: key.toUpperCase(), color: ChartPalette.categorical(index, dark), values: status?.[key] ?? EMPTY_VALUES,
  })), [status, dark]);
  const card = { times: status?.t ?? EMPTY_VALUES, loading: status === null, dark, xAxisName: chartTimeLabel, noDataYet: copy.noDataYet };

  return <Grid variant="2up" gap="base">
    <SeriesCard {...card} title={requests} series={requestSeries} legendValue={(values) => format(sum(values))}
      ariaDescription={`${copy.successful}, ${copy.restricted}, ${copy.failed}`} />
    <SeriesCard {...card} title={responseTime} series={percentileSeries} legendValue={(values) => format(latest(values))}
      ariaDescription={`${responseTime}: ${percentileSeries.map(({ name }) => name).join(", ")}`} />
  </Grid>;
}
