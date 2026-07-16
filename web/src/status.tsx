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

export type Status = { t?: number[]; resolved?: number[]; restricted?: number[]; failed?: number[]; latency?: number[] };

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
    avg: string;
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

export default function StatusCharts({ status, dark, lang, requests, responseTime, copy }: StatusChartsProps) {
  const times = status?.t ?? EMPTY_VALUES;
  const resolved = status?.resolved ?? EMPTY_VALUES;
  const restricted = status?.restricted ?? EMPTY_VALUES;
  const failed = status?.failed ?? EMPTY_VALUES;
  const latency = status?.latency ?? EMPTY_VALUES;
  const numberFormatter = useMemo(() => new Intl.NumberFormat(lang, { maximumFractionDigits: 0 }), [lang]);
  const format = (value: number) => numberFormatter.format(Math.round(value));
  const chartTimeLabel = copy.timeUTC.replace("UTC", Intl.DateTimeFormat().resolvedOptions().timeZone || "Local");
  const successColor = ChartPalette.semantic("Success", dark);
  const restrictedColor = ChartPalette.semantic("Warning", dark);
  const errorColor = ChartPalette.semantic("Attention", dark);
  const neutralColor = ChartPalette.semantic("Neutral", dark);
  const requestSeries = useMemo(() => [
    { name: copy.successful, color: successColor, data: times.map((time, index) => [time * 1000, resolved[index] ?? 0] as [number, number]) },
    { name: copy.restricted, color: restrictedColor, data: times.map((time, index) => [time * 1000, restricted[index] ?? 0] as [number, number]) },
    { name: copy.failed, color: errorColor, data: times.map((time, index) => [time * 1000, failed[index] ?? 0] as [number, number]) },
  ], [times, resolved, restricted, failed, copy.successful, copy.restricted, copy.failed, successColor, restrictedColor, errorColor]);
  const requestsChartRef = useRef<echarts.ECharts>(null);
  const [selection, setSelection] = useState<{ dark: boolean; name: string | null }>({ dark, name: null });
  const isolated = selection.dark === dark ? selection.name : null;

  function toggleSeries(name: string) {
    const chart = requestsChartRef.current;
    if (!chart) return;
    const restore = isolated === name;
    for (const series of requestSeries) {
      chart.dispatchAction({ type: restore || series.name === name ? "legendSelect" : "legendUnSelect", name: series.name });
    }
    setSelection({ dark, name: restore ? null : name });
  }

  const latencySeries = useMemo(() => [
    { name: copy.avg, data: times.map((time, index) => [time * 1000, latency[index] ?? 0] as [number, number]), color: neutralColor },
  ], [times, latency, copy.avg, neutralColor]);

  return <Grid variant="2up" gap="base">
    <GridItem className="min-w-0"><LayerCard>
      <LayerCard.Secondary>{requests}</LayerCard.Secondary>
      <LayerCard.Primary>
        <div className="flex divide-x divide-kumo-line px-2 mb-2 overflow-x-auto">
          <ChartLegend.LargeItem name={copy.successful} color={successColor} value={format(sum(resolved))} className="shrink-0 not-first:pl-4"
            inactive={isolated !== null && isolated !== copy.successful} onClick={() => toggleSeries(copy.successful)} />
          <ChartLegend.LargeItem name={copy.restricted} color={restrictedColor} value={format(sum(restricted))} className="shrink-0 not-first:pl-4"
            inactive={isolated !== null && isolated !== copy.restricted} onClick={() => toggleSeries(copy.restricted)} />
          <ChartLegend.LargeItem name={copy.failed} color={errorColor} value={format(sum(failed))} className="shrink-0 not-first:pl-4"
            inactive={isolated !== null && isolated !== copy.failed} onClick={() => toggleSeries(copy.failed)} />
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
        <div className="flex divide-x divide-kumo-line px-2 mb-2">
          <ChartLegend.LargeItem name={copy.avg} color={neutralColor} value={format(latest(latency))} unit={copy.ms} />
        </div>
        <TimeseriesChart xAxisName={chartTimeLabel} echarts={chartEcharts} isDarkMode={dark} data={latencySeries} height={300}
          loading={status === null} incomplete={incompleteAfter(times)} ariaDescription={responseTime} />
      </LayerCard.Primary>
    </LayerCard></GridItem>
  </Grid>;
}
