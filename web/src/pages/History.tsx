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
  hourly_heatmap: number[][]; // 7 × 24, raw tokens
  pct_burned_heatmap: number[][]; // 7 × 24, sum of positive session-pct deltas
};

const WINDOW_OPTIONS = [
  { label: '24h', days: 1 },
  { label: '7d', days: 7 },
  { label: '30d', days: 30 },
];

const SESSION_COLOR = '#f43f5e';
const SESSION_FAINT = 'rgba(244,63,94,0.35)';
const WEEK_COLOR = '#0ea5e9';
const WEEK_FAINT = 'rgba(14,165,233,0.35)';

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
            disabled={loading}
            className="flex items-center gap-1.5 text-xs text-zinc-500 hover:text-zinc-900 dark:hover:text-zinc-100 transition disabled:opacity-60"
          >
            <RefreshCw className={`size-3.5 ${loading ? 'animate-spin' : ''}`} />
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
        <div className="space-y-8">
          <Section
            title="/usage % over time"
            subtitle={`${data.observations.length} observations · ${data.window_days}d window`}
          >
            <div className="grid gap-5 grid-cols-1 xl:grid-cols-2">
              <UsageCard
                label="Session (5h)"
                ts={usageSeries?.ts ?? []}
                values={usageSeries?.sess ?? []}
                color={SESSION_COLOR}
              />
              <UsageCard
                label="Week"
                ts={usageSeries?.ts ?? []}
                values={usageSeries?.week ?? []}
                color={WEEK_COLOR}
              />
            </div>
          </Section>

          <Section
            title="Tokens per 1%"
            subtitle={
              <>
                How many tokens it took to move each bucket 1 percentage point
                — i.e. roughly{' '}
                <em className="text-zinc-600 dark:text-zinc-300 not-italic font-medium">
                  the price of 1%
                </em>
                . Tokens are{' '}
                <em className="not-italic font-medium text-zinc-600 dark:text-zinc-300">
                  cost-weighted
                </em>{' '}
                using Anthropic's API price ratios (input ×1, output ×5,
                cache_read ×0.1, cache_create_5m ×1.25, cache_create_1h ×2),
                so this tracks billable cost rather than raw token count.
                Saturated observations are excluded; spikes are usually 1%
                jumps where a single heavy turn dominated the window — the
                bold line is a rolling-5 median.
              </>
            }
          >
            <div className="grid gap-5 grid-cols-1 xl:grid-cols-2">
              <CalCard
                label="Session (5h)"
                points={data.session_calibration}
                medianCW={data.latest_session_median_cw}
                medianN={data.latest_session_median_n}
                color={SESSION_COLOR}
                colorFaint={SESSION_FAINT}
              />
              <CalCard
                label="Week"
                points={data.week_calibration}
                medianCW={data.latest_week_median_cw}
                medianN={data.latest_week_median_n}
                color={WEEK_COLOR}
                colorFaint={WEEK_FAINT}
              />
            </div>
          </Section>

          <Section
            title="When you spend"
            subtitle="Two views of the same hours. Tokens shows when you push volume; % burned shows when you actually eat into the session limit. Cache-heavy windows can be loud on tokens but cheap in %, and vice versa."
          >
            <div className="grid gap-5 grid-cols-1 xl:grid-cols-2">
              <Card
                title="Tokens (raw) per weekday × hour"
                subtitle="Sum of input + output + cache tokens, regardless of saturation."
              >
                <Heatmap
                  data={data.hourly_heatmap}
                  rgb="244,63,94"
                  formatValue={(v) => `${fmtNumber(v)} tokens`}
                />
              </Card>
              <Card
                title="% burned per weekday × hour"
                subtitle="Sum of positive session-pct moves. Saturated periods and the bracketed deltas across resets are skipped — they wouldn't represent real burn."
              >
                <Heatmap
                  data={data.pct_burned_heatmap}
                  rgb="14,165,233"
                  formatValue={(v) => `${v}%`}
                />
              </Card>
            </div>
          </Section>
        </div>
      )}
    </div>
  );
}

function Section({
  title,
  subtitle,
  children,
}: {
  title: string;
  subtitle?: React.ReactNode;
  children: React.ReactNode;
}) {
  return (
    <div>
      <div className="mb-3">
        <div className="text-sm font-medium text-zinc-700 dark:text-zinc-300">{title}</div>
        {subtitle && (
          <div className="text-xs text-zinc-500 mt-1 max-w-3xl">{subtitle}</div>
        )}
      </div>
      {children}
    </div>
  );
}

function UsageCard({
  label,
  ts,
  values,
  color,
}: {
  label: string;
  ts: number[];
  values: (number | null)[];
  color: string;
}) {
  const hasData = ts.length > 1 && values.some((v) => v !== null);
  return (
    <div className="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
      <div className="text-xs uppercase tracking-wider text-zinc-500 mb-3">{label}</div>
      {hasData ? (
        <Spark
          ts={ts}
          yRange={[0, 100]}
          ySuffix="%"
          height={200}
          series={[{ label: '%', values, color, width: 1.5 }]}
        />
      ) : (
        <Empty />
      )}
    </div>
  );
}

function CalCard({
  label,
  points,
  medianCW,
  medianN,
  color,
  colorFaint,
}: {
  label: string;
  points: CalibrationPoint[];
  medianCW?: number;
  medianN: number;
  color: string;
  colorFaint: string;
}) {
  const data = useMemo(() => {
    if (points.length === 0) return null;
    const ts = points.map((p) => p.b_ts_unix_ms / 1000);
    const values = points.map((p) => p.tokens_per_pct_cw);
    return { ts, values, median: rollingMedian(values, 5) };
  }, [points]);

  return (
    <div className="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
      <div className="flex items-baseline justify-between gap-3 mb-4">
        <div className="flex flex-col">
          <span className="text-xs uppercase tracking-wider text-zinc-500">{label}</span>
          <span className="text-[10px] text-zinc-500 mt-0.5">
            {medianCW != null && medianN > 0
              ? `Latest median, n=${medianN}`
              : 'No median yet'}
          </span>
        </div>
        {medianCW != null && medianN > 0 && (
          <div className="text-right">
            <span className="text-4xl font-semibold tabular-nums text-zinc-800 dark:text-zinc-100">
              {fmtNumber(medianCW)}
            </span>
            <span className="text-sm text-zinc-500 ml-1">/ 1%</span>
          </div>
        )}
      </div>
      {data && data.ts.length > 1 ? (
        <Spark
          ts={data.ts}
          height={220}
          series={[
            {
              label: 'observed',
              values: data.values,
              color: colorFaint,
              width: 1,
            },
            {
              label: 'rolling median (5)',
              values: data.median,
              color,
              width: 1.75,
              spanGaps: true,
            },
          ]}
          hLines={
            medianCW != null
              ? [
                  {
                    value: medianCW,
                    color: 'rgba(113,113,122,0.55)',
                    dash: [2, 4],
                    label: 'median',
                  },
                ]
              : undefined
          }
        />
      ) : (
        <Empty hint="Need 2+ adjacent non-saturated observations in this bucket before this fills in." />
      )}
    </div>
  );
}

/** Centered rolling median. Window=5 by default; emits null until at least
 *  3 values are available. Lets the chart show a stable line that ignores
 *  one-off spikes (e.g. a 1% jump that captured a heavy cache_create_1h). */
function rollingMedian(values: number[], window: number): (number | null)[] {
  const n = values.length;
  const out: (number | null)[] = new Array(n).fill(null);
  const half = Math.floor(window / 2);
  for (let i = 0; i < n; i++) {
    const lo = Math.max(0, i - half);
    const hi = Math.min(n, i + half + 1);
    const slice = values.slice(lo, hi).slice().sort((a, b) => a - b);
    if (slice.length < Math.min(3, window)) continue;
    const mid = slice.length >> 1;
    out[i] = slice.length % 2 ? slice[mid] : (slice[mid - 1] + slice[mid]) / 2;
  }
  return out;
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

function Heatmap({
  data,
  rgb,
  formatValue,
}: {
  data: number[][];
  /** "r,g,b" used to tint each cell. */
  rgb: string;
  formatValue: (v: number) => string;
}) {
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
                        ? { backgroundColor: `rgba(${rgb},${opacity})` }
                        : undefined
                    }
                    title={`${WEEKDAYS[day]} ${h}:00 — ${formatValue(v)}`}
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
