import { botPattern } from "../shared/routes";

export type RequestMeta = {
  cacheHit: boolean;
  cacheTier?: "edge" | "model" | "origin";
  requestId: string;
  metricStatus?: number;
  errorType?: string;
  metric?: string;
  // Time spent inside the Go server, from its own clock. The gap between
  // duration_ms and this is worker<->container transport + queueing.
  originMs?: number;
};

const SERIES_FIELDS = ["t", "p50", "p90", "p99", "resolved", "restricted", "failed"] as const;
type StatusSeries = Record<(typeof SERIES_FIELDS)[number], number[]>;

// blob1 (route metric) maps to a content category. "posts" covers post/reel
// embed + direct routes; historical rows logged before the "story" metric
// existed fall into "posts" too.
const STATUS_CATEGORIES = ["all", "posts", "stories", "profile"] as const;
type StatusCategory = (typeof STATUS_CATEGORIES)[number];
export type StatusReport = Record<StatusCategory, StatusSeries>;

type StatusRow = Record<string, unknown>;

// Two statements, two grouping levels: per-category plus an "all" rollup.
// Percentiles can't be summed from per-category values, so "all" is aggregated
// server-side over every row rather than derived on the client. The Analytics
// Engine SQL dialect supports neither UNION ALL nor multiIf, so this has to be
// two separate queries using nested if().
const statusSelect = (category: string, groupBy: string) =>
  "SELECT intDiv(toUInt32(timestamp), 600) * 600 AS t, " +
  `${category} AS category, ` +
  "quantileExactWeighted(0.50)(double1, _sample_interval) AS p50, " +
  "quantileExactWeighted(0.90)(double1, _sample_interval) AS p90, " +
  "quantileExactWeighted(0.99)(double1, _sample_interval) AS p99, " +
  "sumIf(_sample_interval, blob3 = 'ok') AS resolved, " +
  "sumIf(_sample_interval, blob3 = 'fail' AND blob4 = 'geo_block_required') AS restricted, " +
  "sumIf(_sample_interval, blob3 = 'fail' AND blob4 != 'geo_block_required') AS failed " +
  "FROM oginstagram_requests WHERE timestamp > NOW() - INTERVAL '1' DAY AND blob5 NOT IN ('edge', 'model') " +
  groupBy;

const STATUS_QUERIES = [
  statusSelect(
    "if(blob1 = 'profile', 'profile', if(blob1 = 'story', 'stories', 'posts'))",
    "GROUP BY t, category"
  ) + " ORDER BY t, category",
  statusSelect("'all'", "GROUP BY t") + " ORDER BY t",
];

export function logRequestMetric(
  request: Request,
  url: URL,
  meta: RequestMeta,
  ms: number,
  status: number,
  ok: boolean,
  ae?: AnalyticsEngineDataset
): void {
  const route = meta.metric;
  if (!route) return;
  const client = clientClass(request.headers.get("user-agent") ?? "");
  if (client === "human") return;

  const outcome = ok ? "ok" : "fail";
  const cache = meta.cacheTier ?? (meta.cacheHit ? "model" : "origin");
  const reason = meta.errorType || (ok ? "ok" : "fail");

  // Workers already records every invocation and automatically traces Worker
  // subrequests. Keep only final server failures and a small successful sample;
  // Analytics Engine still receives every point and samples independently.
  if (status >= 500 || (ok && Math.random() < 0.01)) {
    const entry: Record<string, unknown> = {
      event: "http_request",
      request_id: meta.requestId,
      route,
      client,
      outcome,
      duration_ms: ms,
      cache,
      "http.request.method": request.method,
      "http.response.status_code": status,
      "url.path": url.pathname,
    };
    if (meta.originMs !== undefined) entry.origin_duration_ms = meta.originMs;
    if (!ok) entry["error.type"] = reason;
    (status >= 500 ? console.error : console.info)(entry);
  }

  ae?.writeDataPoint({
    blobs: [route, client, outcome, reason, cache],
    doubles: [ms, ok ? 1 : 0, status, meta.originMs ?? -1],
    indexes: [route],
  });
}

export async function queryStatus(env: Env): Promise<StatusReport> {
  if (!env.AE_ACCOUNT_ID || !env.AE_API_TOKEN) throw new Error("analytics credentials unavailable");
  const target = `https://api.cloudflare.com/client/v4/accounts/${env.AE_ACCOUNT_ID}/analytics_engine/sql`;
  const rows = (await Promise.all(STATUS_QUERIES.map(async (query) => {
    const upstream = await fetch(target, {
      method: "POST",
      headers: { Authorization: `Bearer ${env.AE_API_TOKEN}` },
      body: `${query} FORMAT JSON`,
      signal: AbortSignal.timeout(5000),
    });
    if (!upstream.ok) throw new Error(`analytics upstream ${upstream.status}`);
    const parsed = (await upstream.json()) as { data?: StatusRow[] };
    return parsed.data ?? [];
  }))).flat();
  const report = emptyStatusReport();
  for (const row of rows) {
    const series = report[String(row.category) as StatusCategory];
    if (!series) continue;
    for (const field of SERIES_FIELDS) series[field].push(roundMetric(row[field]));
  }
  return report;
}

export async function serveStatus(env: Env, requestId: string): Promise<Response> {
  if (!env.AE_ACCOUNT_ID || !env.AE_API_TOKEN) {
    return Response.json(emptyStatusReport(), { status: 503, headers: { "cache-control": "no-store" } });
  }
  try {
    return Response.json(await queryStatus(env), {
      headers: {
        "cache-control": "public, max-age=0",
        "cloudflare-cdn-cache-control":
          "public, max-age=60, stale-while-revalidate=60, stale-if-error=300",
      },
    });
  } catch (error) {
    console.error({
      event: "status_query_failed",
      request_id: requestId,
      "error.type": "analytics_query_failed",
      "exception.message": error instanceof Error ? error.message : String(error),
    });
    return Response.json(emptyStatusReport(), {
      status: 502,
      headers: { "cache-control": "no-store" },
    });
  }
}

function emptySeries(): StatusSeries {
  const series = {} as StatusSeries;
  for (const field of SERIES_FIELDS) series[field] = [];
  return series;
}

export function emptyStatusReport(): StatusReport {
  return { all: emptySeries(), posts: emptySeries(), stories: emptySeries(), profile: emptySeries() };
}

function clientClass(userAgent: string): string {
  if (!botPattern.test(userAgent)) return "human";
  if (/discordbot/i.test(userAgent)) return "discord";
  if (/telegrambot/i.test(userAgent)) return "telegram";
  return "bot";
}

function roundMetric(value: unknown): number {
  const number = Number(value);
  return Number.isFinite(number) ? Math.round(number * 100) / 100 : 0;
}
