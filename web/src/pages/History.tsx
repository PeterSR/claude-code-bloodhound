import { useMemo, useState } from 'react';
import { History as HistoryIcon, RefreshCw } from 'lucide-react';
import Spark from '../components/Spark';
import { useApi } from '../hooks/useApi';
import { fmtNumber } from '../lib/format';

type ObservationPoint = {
  ts_unix_ms: number;
  session_pct?: number;
  week_pct?: number;
  session_saturated: boolean;
  week_saturated: boolean;
  session_reset_detected: boolean;
  week_reset_detected: boolean;
};

type CalibrationPoint = {
  b_ts_unix_ms: number;
  delta_pct: number;
  gap_s: number;
  raw_tokens: number;
  cw_tokens: number;
  turn_count: number;
  tokens_per_pct_raw: number;
  tokens_per_pct_cw: number;
};

type HistoryResponse = {
  ok: boolean;
  window_days: number;
  observations: ObservationPoint[];
  session_calibration: CalibrationPoint[];
  week_calibration: CalibrationPoint[];
  latest_session_median_cw?: number;
  latest_week_median_cw?: number;
  latest_session_median_n: number;
  latest_week_median_n: number;
  hourly_heatmap: number[][]; // 7 × 24
};

const WINDOW_OPTIONS = [
  { label: '24h', days: 1 },
  { label: '7d', days: 7 },
  { label: '30d', days: 30 },
];

export default function History() {
  const [days, setDays] = useState(7);
  const { data, error, loading, refresh } = useApi<HistoryResponse>(
    `/history?window_days=${days}`,
    60_000,
  );

  const usageSeries = useMemo(() => {
    if (!data) return null;
    const ts = data.observations.map((o) => o.ts_unix_ms / 1000);
    const sess = data.observations.map((o) =>
      o.session_pct === undefined || o.session_pct === null ? null : o.session_pct,
    );
    const week = data.observations.map((o) =>
      o.week_pct === undefined || o.week_pct === null ? null : o.week_pct,
    );
    return { ts, sess, week };
  }, [data]);

  const calSeries = useMemo(() => {
    if (!data) return null;
    // Combine session + week onto a shared timeline, with nulls where the
    // bucket doesn't have a point at that time.
    const allTs = new Set<number>();
    for (const p of data.session_calibration) allTs.add(p.b_ts_unix_ms);
    for (const p of data.week_calibration) allTs.add(p.b_ts_unix_ms);
    const ts = Array.from(allTs).sort((a, b) => a - b);
    const sessByTS = new Map(data.session_calibration.map((p) => [p.b_ts_unix_ms, p]));
    const weekByTS = new Map(data.week_calibration.map((p) => [p.b_ts_unix_ms, p]));
    return {
      ts: ts.map((t) => t / 1000),
      session: ts.map((t) => sessByTS.get(t)?.tokens_per_pct_cw ?? null),
      week: ts.map((t) => weekByTS.get(t)?.tokens_per_pct_cw ?? null),
    };
  }, [data]);

  return (
    <div>
      <div className="flex items-center justify-between mb-2">
        <div className="flex items-center gap-2">
          <HistoryIcon className="size-5 text-zinc-600 dark:text-zinc-400" />
          <h1 className="text-2xl font-semibold tracking-tight">History</h1>
        </div>
        <div className="flex items-center gap-3">
          <div className="flex rounded-md border border-zinc-300 dark:border-zinc-700 overflow-hidden text-xs">
            {WINDOW_OPTIONS.map((opt) => (
              <button
                key={opt.label}
                onClick={() => setDays(opt.days)}
                className={[
                  'px-3 py-1',
                  days === opt.days
                    ? 'bg-zinc-900 dark:bg-zinc-100 text-white dark:text-zinc-900'
                    : 'bg-white dark:bg-zinc-900 hover:bg-zinc-100 dark:hover:bg-zinc-800',
                ].join(' ')}
              >
                {opt.label}
              </button>
            ))}
          </div>
          <button
            onClick={refresh}
            className="flex items-center gap-1.5 text-xs text-zinc-500 hover:text-zinc-900 dark:hover:text-zinc-100 transition"
          >
            <RefreshCw className="size-3.5" />
            Refresh
          </button>
        </div>
      </div>
      <p className="text-zinc-600 dark:text-zinc-400 max-w-2xl mb-6">
        How your usage trends over time. Calibration converts the opaque
        <code className="font-mono text-xs mx-1">/usage</code>
        percentage into a tokens-per-1% estimate so you can budget by tokens.
      </p>

      {error && (
        <div className="text-sm text-red-600 dark:text-red-400 mb-4">{error.message}</div>
      )}
      {loading && !data && <div className="text-sm text-zinc-500">Loading…</div>}

      {data && (
        <div className="space-y-6 max-w-5xl">
          <Card title="/usage % over time" subtitle={`${data.observations.length} observations · ${data.window_days}d window`}>
            {usageSeries && usageSeries.ts.length > 1 ? (
              <Spark
                ts={usageSeries.ts}
                yRange={[0, 100]}
                ySuffix="%"
                series={[
                  { label: 'Session', values: usageSeries.sess, color: '#f43f5e' },
                  { label: 'Week', values: usageSeries.week, color: '#0ea5e9' },
                ]}
              />
            ) : (
              <Empty />
            )}
          </Card>

          <Card
            title="Tokens per 1% (cost-weighted)"
            subtitle={
              <>
                Calibration: how many cost-weighted tokens it took to move the
                bucket 1 percentage point. Saturated observations are excluded.
                Latest median (n=
                {data.latest_session_median_n}):{' '}
                <strong className="text-zinc-700 dark:text-zinc-300">
                  {data.latest_session_median_cw
                    ? fmtNumber(data.latest_session_median_cw) + ' / 1%'
                    : '—'}
                </strong>{' '}
                session,{' '}
                <strong className="text-zinc-700 dark:text-zinc-300">
                  {data.latest_week_median_cw
                    ? fmtNumber(data.latest_week_median_cw) + ' / 1%'
                    : '—'}
                </strong>{' '}
                week.
              </>
            }
          >
            {calSeries && calSeries.ts.length > 1 ? (
              <Spark
                ts={calSeries.ts}
                series={[
                  { label: 'Session', values: calSeries.session, color: '#f43f5e' },
                  { label: 'Week', values: calSeries.week, color: '#0ea5e9' },
                ]}
              />
            ) : (
              <Empty hint="At least two non-saturated observations in the same bucket are needed before this fills in." />
            )}
          </Card>

          <Card title="When you spend" subtitle="Raw tokens per weekday × hour. Darker = more tokens.">
            <Heatmap data={data.hourly_heatmap} />
          </Card>
        </div>
      )}
    </div>
  );
}

function Card({
  title,
  subtitle,
  children,
}: {
  title: string;
  subtitle?: React.ReactNode;
  children: React.ReactNode;
}) {
  return (
    <div className="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
      <div className="text-sm font-medium text-zinc-700 dark:text-zinc-300">{title}</div>
      {subtitle && (
        <div className="text-xs text-zinc-500 mt-1 mb-4">{subtitle}</div>
      )}
      {children}
    </div>
  );
}

function Empty({ hint }: { hint?: string }) {
  return (
    <div className="text-sm text-zinc-500 text-center py-12">
      Not enough data yet.
      {hint && <div className="text-xs text-zinc-400 mt-1">{hint}</div>}
    </div>
  );
}

const WEEKDAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];

function Heatmap({ data }: { data: number[][] }) {
  const max = Math.max(1, ...data.flat());
  return (
    <div className="overflow-x-auto">
      <table className="text-[10px] tabular-nums">
        <thead>
          <tr>
            <th className="w-10"></th>
            {Array.from({ length: 24 }, (_, h) => (
              <th key={h} className="px-0.5 text-center text-zinc-500 font-normal">
                {h % 3 === 0 ? h : ''}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {data.map((row, day) => (
            <tr key={day}>
              <td className="pr-2 text-right text-zinc-500">{WEEKDAYS[day]}</td>
              {row.map((v, h) => {
                const intensity = v / max;
                const bg = intensity === 0
                  ? 'bg-zinc-100 dark:bg-zinc-800'
                  : '';
                const opacity = intensity === 0 ? 0 : Math.max(0.15, intensity);
                return (
                  <td
                    key={h}
                    className={`w-5 h-4 ${bg}`}
                    style={
                      intensity > 0
                        ? { backgroundColor: `rgba(244,63,94,${opacity})` }
                        : undefined
                    }
                    title={`${WEEKDAYS[day]} ${h}:00 — ${fmtNumber(v)} tokens`}
                  />
                );
              })}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
