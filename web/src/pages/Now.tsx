import { useMemo } from 'react';
import { Activity, AlertCircle, AlertTriangle, RefreshCw, Zap } from 'lucide-react';
import Spark from '../components/Spark';
import { useApi } from '../hooks/useApi';
import { fmtAbs, fmtDuration, fmtRel, pctColor } from '../lib/format';

type WindowState = {
  pct: number;
  reset_ts?: string;
  window_start_ts?: string;
  time_to_reset_ms?: number;
  burn_pct_per_hour?: number;
  burn_ok: boolean;
  limit_ok: boolean;
  limit_eta_ms?: number;
  limit_eta_ts?: string;
  reset_detected_in_last_obs: boolean;
  saturated: boolean;
};

type HistoryPoint = {
  ts_unix_ms: number;
  pct?: number;
  saturated: boolean;
};

type LastPoll = {
  ts: string;
  age_s: number;
  parse_ok: boolean;
  elapsed_s: number;
};

type NowResponse = {
  ok: boolean;
  session: WindowState | null;
  week: WindowState | null;
  last_poll: LastPoll | null;
  server_now_ms: number;
  poll_interval_s?: number;
  stale_after_s?: number;
  session_history?: HistoryPoint[];
  week_history?: HistoryPoint[];
};

export default function Now() {
  const { data, error, loading, refresh } = useApi<NowResponse>('/now', 30_000);

  return (
    <div>
      <div className="flex items-center justify-between mb-2">
        <div className="flex items-center gap-2">
          <Activity className="size-5 text-rose-500" />
          <h1 className="text-2xl font-semibold tracking-tight">Now</h1>
        </div>
        <button
          onClick={refresh}
          disabled={loading}
          className="flex items-center gap-1.5 text-xs text-zinc-500 hover:text-zinc-900 dark:hover:text-zinc-100 transition disabled:opacity-60"
          title="Refresh"
        >
          <RefreshCw className={`size-3.5 ${loading ? 'animate-spin' : ''}`} />
          Refresh
        </button>
      </div>
      <p className="text-zinc-600 dark:text-zinc-400 max-w-2xl mb-6">
        Where you stand right now. Windows are anchored to the actual reset
        time reported by Claude Code's <code className="font-mono text-xs">/usage</code> panel.
      </p>

      {error && <ErrorBanner error={error} />}

      {loading && !data && (
        <div className="text-sm text-zinc-500 py-12 text-center">Loading…</div>
      )}

      {data && (
        <>
          <PollBanner poll={data.last_poll} />
          <div className="grid gap-5 grid-cols-1 xl:grid-cols-2 mt-4">
            <WindowCard
              label="Session (5h)"
              window={data.session}
              points={data.session_history}
              color="#f43f5e"
              nowMS={data.server_now_ms}
              fitWindowS={30 * 60}
              pollAgeS={data.last_poll?.age_s}
              pollIntervalS={data.poll_interval_s}
            />
            <WindowCard
              label="Week"
              window={data.week}
              points={data.week_history}
              color="#0ea5e9"
              nowMS={data.server_now_ms}
              fitWindowS={2 * 3600}
              pollAgeS={data.last_poll?.age_s}
              pollIntervalS={data.poll_interval_s}
            />
          </div>
        </>
      )}
    </div>
  );
}

function WindowCard({
  label,
  window: w,
  points,
  color,
  nowMS,
  fitWindowS,
  pollAgeS,
  pollIntervalS,
}: {
  label: string;
  window: WindowState | null;
  points?: HistoryPoint[];
  color: string;
  nowMS: number;
  fitWindowS: number;
  /** Age of the most recent /usage poll in seconds. */
  pollAgeS?: number;
  /** Configured poll interval in seconds. */
  pollIntervalS?: number;
}) {
  const chart = useMemo(() => {
    if (!w || !w.window_start_ts || !w.reset_ts) return null;
    const startMS = new Date(w.window_start_ts).getTime();
    const resetMS = new Date(w.reset_ts).getTime();
    if (!Number.isFinite(startMS) || !Number.isFinite(resetMS)) return null;
    const obs = points ?? [];

    const startS = startMS / 1000;
    const resetS = resetMS / 1000;
    const nowS = Math.min(Math.max(nowMS / 1000, startS), resetS);

    const obsValid = obs.filter(
      (p) => p.pct !== undefined && p.pct !== null,
    ) as Array<HistoryPoint & { pct: number }>;
    const lastObs = obsValid.length ? obsValid[obsValid.length - 1] : null;

    let fit:
      | { slopePctPerH: number; fromS: number; toS: number; n: number }
      | null = null;
    if (lastObs) {
      const lastS = lastObs.ts_unix_ms / 1000;
      const cutS = lastS - fitWindowS;
      let slice = obsValid.filter((p) => p.ts_unix_ms / 1000 >= cutS);
      if (slice.length < 3) slice = obsValid.slice(-3);
      if (slice.length >= 2) {
        const n = slice.length;
        const x0 = slice[0].ts_unix_ms / 1000;
        let sumX = 0,
          sumY = 0,
          sumXY = 0,
          sumXX = 0;
        for (const p of slice) {
          const x = (p.ts_unix_ms / 1000 - x0) / 3600;
          const y = p.pct;
          sumX += x;
          sumY += y;
          sumXY += x * y;
          sumXX += x * x;
        }
        const denom = n * sumXX - sumX * sumX;
        if (denom > 1e-9) {
          fit = {
            slopePctPerH: (n * sumXY - sumX * sumY) / denom,
            fromS: slice[0].ts_unix_ms / 1000,
            toS: lastS,
            n,
          };
        }
      }
    }

    const projPoints: Array<{ tsS: number; pct: number }> = [];
    let limitS: number | null = null;
    if (lastObs && fit) {
      const lastS = lastObs.ts_unix_ms / 1000;
      const lastPct = lastObs.pct;
      const slope = fit.slopePctPerH;
      projPoints.push({ tsS: lastS, pct: lastPct });
      if (slope > 0 && lastPct < 100) {
        const hoursToCap = (100 - lastPct) / slope;
        const capS = lastS + hoursToCap * 3600;
        if (capS > lastS && capS < resetS) {
          limitS = capS;
          projPoints.push({ tsS: capS, pct: 100 });
          projPoints.push({ tsS: resetS, pct: 100 });
        }
      }
      if (limitS === null) {
        const dtH = (resetS - lastS) / 3600;
        const endPct = Math.min(100, Math.max(0, lastPct + slope * dtH));
        if (resetS > lastS) {
          projPoints.push({ tsS: resetS, pct: endPct });
        }
      }
    }

    const tsSet = new Set<number>([startS, resetS]);
    for (const p of obs) tsSet.add(p.ts_unix_ms / 1000);
    for (const p of projPoints) tsSet.add(p.tsS);
    if (nowS > startS && nowS < resetS) tsSet.add(nowS);
    const ts = Array.from(tsSet).sort((a, b) => a - b);

    const obsByTS = new Map<number, number | null>();
    for (const p of obs) obsByTS.set(p.ts_unix_ms / 1000, p.pct ?? null);
    const projByTS = new Map<number, number>();
    for (const p of projPoints) projByTS.set(p.tsS, p.pct);

    const actualValues: (number | null)[] = ts.map((t) =>
      obsByTS.has(t) ? obsByTS.get(t) ?? null : null,
    );
    const projValues: (number | null)[] = ts.map((t) =>
      projByTS.has(t) ? projByTS.get(t)! : null,
    );

    const xBands: Array<{ from: number; to: number; color: string }> = [];
    let runStart: number | null = null;
    let runPrev: number | null = null;
    for (const p of obs) {
      const t = p.ts_unix_ms / 1000;
      if (p.saturated) {
        if (runStart === null) runStart = t;
        runPrev = t;
      } else if (runStart !== null && runPrev !== null) {
        xBands.push({ from: runStart, to: runPrev, color: 'rgba(244,63,94,0.10)' });
        runStart = null;
        runPrev = null;
      }
    }
    if (runStart !== null && runPrev !== null) {
      xBands.push({ from: runStart, to: nowS, color: 'rgba(244,63,94,0.10)' });
    }

    return {
      ts,
      actualValues,
      projValues,
      count: obs.length,
      startMS,
      resetMS,
      nowS,
      limitS,
      currentPct: w.pct,
      hasProjection: projPoints.length >= 2,
      xBands,
      fit,
    };
  }, [w, points, nowMS, fitWindowS]);

  const seriesList = chart
    ? [
        { label: 'pct', values: chart.actualValues, color, width: 1.75 },
        ...(chart.hasProjection
          ? [
              {
                label: 'projected',
                values: chart.projValues,
                color,
                dash: [4, 4],
                width: 1.25,
                // Endpoints sit on lastObsS and resetS (or the cap), but
                // ts entries between them (notably "now") are null in this
                // series. Bridge them so the projection draws as a line.
                spanGaps: true,
              },
            ]
          : []),
      ]
    : [];

  // Show the "now" marker only when the latest poll is older than the
  // configured cadence. Within one poll interval the line would sit
  // basically on top of the last observation — redundant.
  const freshThresholdS = pollIntervalS ?? 300;
  const pollIsFresh = pollAgeS !== undefined && pollAgeS <= freshThresholdS;

  const vLines = chart
    ? [
        ...(!pollIsFresh && chart.nowS > chart.startMS / 1000 && chart.nowS < chart.resetMS / 1000
          ? [{ x: chart.nowS, color: '#71717a', dash: [2, 3], label: 'now' }]
          : []),
        ...(chart.limitS !== null
          ? [{ x: chart.limitS, color: '#dc2626', dash: [4, 4], label: '→ 100%', width: 1.5 }]
          : []),
      ]
    : [];

  const hLines =
    chart && chart.currentPct > 0
      ? [
          {
            value: chart.currentPct,
            color: 'rgba(113,113,122,0.6)',
            dash: [2, 4],
          },
        ]
      : [];

  if (!w) {
    return (
      <div className="rounded-lg border border-dashed border-zinc-300 dark:border-zinc-700 p-6">
        <div className="text-xs uppercase tracking-wider text-zinc-500">{label}</div>
        <div className="mt-2 text-zinc-500">No data yet.</div>
      </div>
    );
  }

  const clampedPct = Math.max(0, Math.min(100, w.pct));
  // Time-to-100% from the client-side fit, used for the warning banner.
  const limitETAMS =
    chart?.limitS != null ? chart.limitS * 1000 - nowMS : undefined;
  const limitETATS =
    chart?.limitS != null
      ? new Date(chart.limitS * 1000).toISOString()
      : undefined;

  return (
    <div className="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-6">
      <div className="flex items-baseline justify-between gap-3 mb-4">
        <div className="flex items-center gap-2">
          <span className="text-xs uppercase tracking-wider text-zinc-500">{label}</span>
          {w.saturated && (
            <span
              className="inline-flex items-center gap-1 rounded-full bg-red-500/15 text-red-700 dark:text-red-300 px-2 py-0.5 text-[10px] font-medium uppercase tracking-wider"
              title="At cap — additional spend is on Anthropic's pay-per-use Extra usage tier. Pct stops moving here."
            >
              <Zap className="size-3" />
              On extra usage
            </span>
          )}
        </div>
        <span className={`text-5xl font-semibold tabular-nums ${pctColor(clampedPct)}`}>
          {clampedPct}%
        </span>
      </div>

      <div className="flex flex-wrap items-baseline gap-x-6 gap-y-1 mb-3 text-sm">
        <div>
          <span className="text-[10px] uppercase tracking-wider text-zinc-500 mr-2">
            Time to reset
          </span>
          {w.time_to_reset_ms !== undefined && w.time_to_reset_ms > 0 ? (
            <span className="tabular-nums">{fmtDuration(w.time_to_reset_ms)}</span>
          ) : (
            <span className="text-zinc-500">—</span>
          )}
          {w.reset_ts && (
            <span className="text-zinc-500 ml-2">at {fmtAbs(w.reset_ts)}</span>
          )}
        </div>
      </div>

      {limitETAMS !== undefined && limitETAMS > 0 && (
        <div className="mb-4 flex items-center gap-1.5 text-amber-700 dark:text-amber-400 bg-amber-50 dark:bg-amber-950/40 border border-amber-200 dark:border-amber-900 rounded-md px-3 py-1.5 text-sm">
          <AlertTriangle className="size-3.5 shrink-0" />
          <span>
            On track to hit 100% in{' '}
            <strong className="tabular-nums">{fmtDuration(limitETAMS)}</strong>
            {limitETATS && (
              <span className="text-amber-600 dark:text-amber-500/80">
                {' '}
                ({fmtAbs(limitETATS)})
              </span>
            )}
          </span>
        </div>
      )}

      {chart && chart.ts.length > 2 ? (
        <Spark
          ts={chart.ts}
          yRange={[0, 100]}
          ySuffix="%"
          height={260}
          series={seriesList}
          vLines={vLines}
          hLines={hLines}
          xBands={chart.xBands}
        />
      ) : (
        <div className="text-sm text-zinc-500 text-center py-12">
          Window known, but no observations yet.
        </div>
      )}

      <div className="mt-3 flex flex-wrap items-baseline gap-x-6 gap-y-1 text-xs text-zinc-500">
        {w.window_start_ts && w.reset_ts && (
          <div>
            <span className="uppercase tracking-wider mr-2">Window</span>
            <span className="tabular-nums">
              {fmtAbs(w.window_start_ts)} → {fmtAbs(w.reset_ts)}
            </span>
          </div>
        )}
        {chart?.fit && (
          <div>
            <span className="uppercase tracking-wider mr-2">Projection</span>
            <span className="tabular-nums">
              recent {fmtFitSpan(chart.fit.toS - chart.fit.fromS)} ({chart.fit.n} obs) ·{' '}
              {chart.fit.slopePctPerH >= 0 ? '+' : ''}
              {chart.fit.slopePctPerH.toFixed(1)}%/h
            </span>
          </div>
        )}
      </div>
    </div>
  );
}

function fmtFitSpan(s: number) {
  if (s < 60) return `${Math.round(s)}s`;
  if (s < 3600) return `${Math.round(s / 60)}m`;
  return `${(s / 3600).toFixed(1)}h`;
}

function PollBanner({ poll }: { poll: LastPoll | null }) {
  if (!poll) {
    return (
      <div className="rounded-md border border-amber-300 dark:border-amber-700 bg-amber-50 dark:bg-amber-950/40 px-4 py-2 text-sm">
        <span className="font-medium">No /usage observations yet.</span>{' '}
        <span className="text-zinc-600 dark:text-zinc-400">
          Run <code className="font-mono text-xs">bloodhound poll</code> or enable the daemon.
        </span>
      </div>
    );
  }
  const stale = poll.age_s > 600;
  return (
    <div
      className={[
        'rounded-md border px-4 py-2 text-sm',
        stale
          ? 'border-amber-300 dark:border-amber-700 bg-amber-50 dark:bg-amber-950/40'
          : 'border-zinc-200 dark:border-zinc-800 bg-zinc-50 dark:bg-zinc-900',
      ].join(' ')}
    >
      <span className="text-zinc-600 dark:text-zinc-400">Last poll </span>
      <span className="font-medium">{fmtRel(poll.age_s)}</span>{' '}
      <span className="text-zinc-500">({fmtAbs(poll.ts)} · {poll.elapsed_s.toFixed(1)}s)</span>
      {stale && <span className="ml-2 text-amber-700 dark:text-amber-400">— stale</span>}
      {!poll.parse_ok && (
        <span className="ml-2 text-red-600 dark:text-red-400">— extraction failed</span>
      )}
    </div>
  );
}

function ErrorBanner({ error }: { error: Error }) {
  return (
    <div className="rounded-md border border-red-300 dark:border-red-700 bg-red-50 dark:bg-red-950/40 px-4 py-2 text-sm flex items-start gap-2">
      <AlertCircle className="size-4 text-red-600 dark:text-red-400 shrink-0 mt-0.5" />
      <div>
        <div className="font-medium text-red-700 dark:text-red-300">Couldn't load /api/now</div>
        <div className="text-red-700 dark:text-red-400 mt-0.5">{error.message}</div>
      </div>
    </div>
  );
}
