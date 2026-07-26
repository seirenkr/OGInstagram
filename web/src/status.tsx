import { useMemo, useRef, useState } from "react";
import { ChartLegend, ChartPalette, TimeseriesChart } from "@cloudflare/kumo/components/chart";
import { Grid, GridItem } from "@cloudflare/kumo/components/grid";
import { LayerCard } from "@cloudflare/kumo/components/layer-card";
import * as echarts from "echarts/core";
import { LineChart } from "echarts/charts";
import { AriaComponent, AxisPointerComponent, BrushComponent, GridComponent, LegendComponent, ToolboxComponent, TooltipComponent } from "echarts/components";
import { SVGRenderer } from "echarts/renderers";

echarts.use([LineChart, AxisPointerComponent, BrushComponent, GridComponent, LegendComponent, ToolboxComponent, TooltipComponent, AriaComponent, SVGRenderer]);

const chartEcharts = {
  ...echarts,
  init: (dom: Parameters<typeof echarts.init>[0], theme?: Parameters<typeof echarts.init>[1], options?: Parameters<typeof echarts.init>[2]) => {
    const chart = echarts.init(dom, theme, { ...options, renderer: "svg" });
    const fontFamily = getComputedStyle(document.documentElement).getPropertyValue("--font-sans").trim();
    chart.setOption({ useUTC: false, textStyle: { fontFamily } });
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
  copy: {
    successful: string;
    failed: string;
    restricted: string;
    ms: string;
    noDataYet: string;
    timeUTC: string;
  };
};

const EMPTY_VALUES: number[] = [];

function sum(values: number[] = []) { return values.reduce((total, value) => total + value, 0); }
function latest(values: number[] = []) { return values.length ? values[values.length - 1] : 0; }
function incompleteAfter(times: number[]): { after: number } | undefined {
  return times.length >= 2 ? { after: times[times.length - 2] * 1000 } : undefined;
}

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

export default function StatusCharts({ status, dark, lang, requests, responseTime, copy }: StatusChartsProps) {
  const times = status?.t ?? EMPTY_VALUES;
  const resolved = status?.resolved ?? EMPTY_VALUES;
  const restricted = status?.restricted ?? EMPTY_VALUES;
  const failed = status?.failed ?? EMPTY_VALUES;
  const p50 = status?.p50 ?? EMPTY_VALUES;
  const p90 = status?.p90 ?? EMPTY_VALUES;
  const p99 = status?.p99 ?? EMPTY_VALUES;
  const numberFormatter = useMemo(() => new Intl.NumberFormat(lang, { maximumFractionDigits: 0 }), [lang]);
  const format = (value: number) => numberFormatter.format(Math.round(value));
  const chartTimeLabel = copy.timeUTC.replace("UTC",
    new Intl.DateTimeFormat(undefined, { timeZoneName: "shortOffset" })
      .formatToParts().find((part) => part.type === "timeZoneName")?.value || "UTC");
  const successColor = ChartPalette.semantic("Success", dark);
  const restrictedColor = ChartPalette.semantic("Warning", dark);
  const errorColor = ChartPalette.semantic("Attention", dark);
  const p50Color = ChartPalette.categorical(0, dark);
  const p90Color = ChartPalette.categorical(1, dark);
  const p99Color = ChartPalette.categorical(2, dark);
  const requestSeries = useMemo(() => [
    { name: copy.successful, color: successColor, data: times.map((time, index) => [time * 1000, resolved[index] ?? 0] as [number, number]) },
    { name: copy.restricted, color: restrictedColor, data: times.map((time, index) => [time * 1000, restricted[index] ?? 0] as [number, number]) },
    { name: copy.failed, color: errorColor, data: times.map((time, index) => [time * 1000, failed[index] ?? 0] as [number, number]) },
  ], [times, resolved, restricted, failed, copy.successful, copy.restricted, copy.failed, successColor, restrictedColor, errorColor]);
  const {
    chartRef: requestsChartRef, isolated: isolatedRequest, toggle: toggleRequestSeries,
  } = useSeriesIsolation(dark, requestSeries);

  const percentileSeries = useMemo(() => [
    {
      name: "P50",
      data: times.map((time, index) => [time * 1000, p50[index] ?? 0] as [number, number]),
      color: p50Color,
    },
    {
      name: "P90",
      data: times.map((time, index) => [time * 1000, p90[index] ?? 0] as [number, number]),
      color: p90Color,
    },
    {
      name: "P99",
      data: times.map((time, index) => [time * 1000, p99[index] ?? 0] as [number, number]),
      color: p99Color,
    },
  ], [times, p50, p90, p99, p50Color, p90Color, p99Color]);
  const {
    chartRef: percentileChartRef, isolated: isolatedPercentile, toggle: togglePercentileSeries,
  } = useSeriesIsolation(dark, percentileSeries);

  return <Grid variant="2up" gap="base">
    <GridItem className="min-w-0"><LayerCard>
      <LayerCard.Secondary>{requests}</LayerCard.Secondary>
      <LayerCard.Primary>
        <div className="flex flex-wrap gap-4">
          <ChartLegend.SmallItem name={copy.successful} color={successColor} value={format(sum(resolved))}
            inactive={isolatedRequest !== null && isolatedRequest !== copy.successful} onClick={() => toggleRequestSeries(copy.successful)} />
          <ChartLegend.SmallItem name={copy.restricted} color={restrictedColor} value={format(sum(restricted))}
            inactive={isolatedRequest !== null && isolatedRequest !== copy.restricted} onClick={() => toggleRequestSeries(copy.restricted)} />
          <ChartLegend.SmallItem name={copy.failed} color={errorColor} value={format(sum(failed))}
            inactive={isolatedRequest !== null && isolatedRequest !== copy.failed} onClick={() => toggleRequestSeries(copy.failed)} />
        </div>
        {status !== null && !times.length ? <div className="min-h-[300px] grid place-items-center text-kumo-subtle text-[13px]">{copy.noDataYet}</div> :
          <TimeseriesChart ref={requestsChartRef} echarts={chartEcharts} isDarkMode={dark} data={requestSeries}
            height={300} xAxisName={chartTimeLabel} incomplete={incompleteAfter(times)}
            loading={status === null} enableLegendSelection ariaDescription={`${copy.successful}, ${copy.restricted}, ${copy.failed}`} />}
      </LayerCard.Primary>
    </LayerCard></GridItem>
    <GridItem className="min-w-0"><LayerCard>
      <LayerCard.Secondary>{responseTime}</LayerCard.Secondary>
      <LayerCard.Primary>
        <div className="flex flex-wrap gap-4">
          <ChartLegend.SmallItem name="P50" color={p50Color} value={format(latest(p50))} unit={copy.ms}
            inactive={isolatedPercentile !== null && isolatedPercentile !== "P50"} onClick={() => togglePercentileSeries("P50")} />
          <ChartLegend.SmallItem name="P90" color={p90Color} value={format(latest(p90))} unit={copy.ms}
            inactive={isolatedPercentile !== null && isolatedPercentile !== "P90"} onClick={() => togglePercentileSeries("P90")} />
          <ChartLegend.SmallItem name="P99" color={p99Color} value={format(latest(p99))} unit={copy.ms}
            inactive={isolatedPercentile !== null && isolatedPercentile !== "P99"} onClick={() => togglePercentileSeries("P99")} />
        </div>
        {status !== null && !times.length ? <div className="min-h-[300px] grid place-items-center text-kumo-subtle text-[13px]">{copy.noDataYet}</div> :
          <TimeseriesChart ref={percentileChartRef} xAxisName={chartTimeLabel} echarts={chartEcharts} isDarkMode={dark} data={percentileSeries} height={300}
            loading={status === null} incomplete={incompleteAfter(times)}
            enableLegendSelection ariaDescription={`${responseTime}: ${percentileSeries.map(({ name }) => name).join(", ")}`} />}
      </LayerCard.Primary>
    </LayerCard></GridItem>
  </Grid>;
}
