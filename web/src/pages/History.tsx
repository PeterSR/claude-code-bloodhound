import { useMemo, useState } from 'react';
import { History as HistoryIcon } from 'lucide-react';
import Spark from '../components/Spark';
import ReloadButton from '../components/ReloadButton';
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
  // False for a misparsed reading (fell further than rounding allows, then
  // recovered). Charted as a gap, not a dip.
  session_pct_valid: boolean;
  week_pct_valid: boolean;
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

type BurnPoint = {
  ts_unix_ms: number;
  session_pct_per_hour?: number;
  week_pct_per_hour?: number;
};

type HistoryResponse = {
  ok: boolean;
  window_days: number;
  burn_rate: BurnPoint[];
  burn_window_min: number;
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

type SessionSlice = {
  start_unix_ms: number;
  end_unix_ms: number;
  week_pct: number;
};

type WeekCapacity = {
  start_unix_ms: number;
  reset_unix_ms?: number;
  sessions: SessionSlice[];
  total_week_pct: number;
  hit_cap: boolean;
  in_progress?: boolean;
  partial?: boolean;
};

type CapacityResponse = {
  ok: boolean;
  window_weeks: number;
  weeks: WeekCapacity[];
  typical_session_week_pct?: number;
  session_week_pct_p25?: number;
  session_week_pct_p75?: number;
  sessions_per_week?: number;
  days_per_session?: number;
  session_count: number;
  maxed_sessions_per_week?: number;
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

const BURN_OPTIONS = [
  { label: '15m', min: 15 },
  { label: '45m', min: 45 },
  { label: '2h', min: 120 },
];

/** The rate that exactly exhausts a limit window if you sustain it for the
 *  window's whole length. Above this line you are on course to run out
 *  before the reset; below it you are not. */
const SESSION_SUSTAINABLE = 100 / 5; // 5-hour window
const WEEK_SUSTAINABLE = 100 / (7 * 24);

export default function History() {
  const [days, setDays] = useState(7);
  const [burnMin, setBurnMin] = useState(45);
  const { data, error, loading, refreshing, refresh } = useApi<HistoryResponse>(
    `/history?window_days=${days}&burn_window_min=${burnMin}`,
    60_000,
  );
  // Weekly capacity spans several weekly windows, so it fetches on its own
  // fixed horizon rather than following the 24h/7d/30d selector above.
  const { data: cap } = useApi<CapacityResponse>('/capacity?weeks=8', 60_000);

  const sessionUsage = useMemo(
    () =>
      data
        ? breakAtResets(
            data.observations,
            (o) => (o.session_pct_valid ? o.session_pct : null),
            (o) => o.session_reset_detected,
          )
        : null,
    [data],
  );
  const weekUsage = useMemo(
    () =>
      data
        ? breakAtResets(
            data.observations,
            (o) => (o.week_pct_valid ? o.week_pct : null),
            (o) => o.week_reset_detected,
          )
        : null,
    [data],
  );
  const sessionResetTS = useMemo(
    () =>
      data
        ? data.observations
            .filter((o) => o.session_reset_detected)
            .map((o) => o.ts_unix_ms / 1000)
        : [],
    [data],
  );
  const weekResetTS = useMemo(
    () =>
      data
        ? data.observations
            .filter((o) => o.week_reset_detected)
            .map((o) => o.ts_unix_ms / 1000)
        : [],
    [data],
  );

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
          <ReloadButton refreshing={refreshing} onClick={refresh} />
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
                ts={sessionUsage?.ts ?? []}
                values={sessionUsage?.values ?? []}
                color={SESSION_COLOR}
                resetTS={sessionResetTS}
              />
              <UsageCard
                label="Week"
                ts={weekUsage?.ts ?? []}
                values={weekUsage?.values ?? []}
                color={WEEK_COLOR}
                resetTS={weekResetTS}
              />
            </div>
          </Section>

          <Section
            title="Burn rate"
            subtitle={
              <>
                How fast you were spending the limit at each moment — the
                slope of the charts above, in percentage points per hour.
                Measured over a trailing {data.burn_window_min}-minute
                baseline: <code className="font-mono text-xs">/usage</code>{' '}
                reports whole percents, so differentiating adjacent readings
                mostly measures rounding, and a longer baseline divides that
                fixed error down until real signal outweighs it. The line
                breaks where no honest rate exists — across a reset, while
                saturated, or where a reading was misparsed.
              </>
            }
          >
            <div className="flex justify-end mb-3">
              <div className="flex items-center gap-2 text-xs text-zinc-500">
                <span>Smoothing</span>
                <div className="flex rounded-md border border-zinc-300 dark:border-zinc-700 overflow-hidden">
                  {BURN_OPTIONS.map((opt) => (
                    <button
                      key={opt.label}
                      onClick={() => setBurnMin(opt.min)}
                      className={[
                        'px-3 py-1',
                        burnMin === opt.min
                          ? 'bg-zinc-900 dark:bg-zinc-100 text-white dark:text-zinc-900'
                          : 'bg-white dark:bg-zinc-900 hover:bg-zinc-100 dark:hover:bg-zinc-800',
                      ].join(' ')}
                    >
                      {opt.label}
                    </button>
                  ))}
                </div>
              </div>
            </div>
            <div className="grid gap-5 grid-cols-1 xl:grid-cols-2">
              <BurnCard
                label="Session (5h)"
                points={data.burn_rate}
                pick={(p) => p.session_pct_per_hour}
                color={SESSION_COLOR}
                resetTS={sessionResetTS}
                sustainable={SESSION_SUSTAINABLE}
                sustainableHint="Sustain this for the full 5 hours and the window runs out exactly at reset."
              />
              <BurnCard
                label="Week"
                points={data.burn_rate}
                pick={(p) => p.week_pct_per_hour}
                color={WEEK_COLOR}
                resetTS={weekResetTS}
                sustainable={WEEK_SUSTAINABLE}
                sustainableHint="The rate that would exhaust the week if held around the clock. Working in bursts, you spend most of the week well above it and the rest of it at zero."
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
                resetTS={sessionResetTS}
              />
              <CalCard
                label="Week"
                points={data.week_calibration}
                medianCW={data.latest_week_median_cw}
                medianN={data.latest_week_median_n}
                color={WEEK_COLOR}
                colorFaint={WEEK_FAINT}
                resetTS={weekResetTS}
              />
            </div>
          </Section>

          {cap && <CapacitySection cap={cap} />}

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

function CapacitySection({ cap }: { cap: CapacityResponse }) {
  const enough =
    cap.session_count >= 3 &&
    cap.sessions_per_week != null &&
    cap.typical_session_week_pct != null;

  return (
    <Section
      title="Weekly capacity"
      subtitle="How many typical 5h work sessions fit inside your weekly limit, measured from the weekly cost of each session in your history. Not the maxed-out ceiling, just the pace you actually work at. A work session is one that used at least 3% of the weekly limit; briefer check-ins are left out."
    >
      <div className="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
        {enough ? (
          <>
            <div className="grid gap-6 lg:grid-cols-[minmax(0,16rem),1fr] items-center">
              <div>
                <div className="flex items-baseline gap-2">
                  <span className="text-5xl font-semibold tabular-nums text-zinc-800 dark:text-zinc-100">
                    {formatSessions(cap.sessions_per_week!)}
                  </span>
                  <span className="text-sm text-zinc-500">sessions / week</span>
                </div>
                <div className="text-xs text-zinc-500 mt-3 leading-relaxed">
                  A typical work session eats{' '}
                  <span className="font-medium text-zinc-700 dark:text-zinc-300">
                    ~{fmtNumber(cap.typical_session_week_pct!)}%
                  </span>{' '}
                  of your weekly limit
                  {cap.session_week_pct_p25 != null && cap.session_week_pct_p75 != null && (
                    <>
                      {' '}
                      (most land {fmtNumber(cap.session_week_pct_p25)}–
                      {fmtNumber(cap.session_week_pct_p75)}%)
                    </>
                  )}
                  . That's about{' '}
                  <span className="font-medium text-zinc-700 dark:text-zinc-300">
                    one every {formatCadence(cap.days_per_session!)}
                  </span>
                  , across {cap.session_count} work sessions.
                </div>
                {cap.maxed_sessions_per_week != null && (
                  <div className="text-[11px] text-zinc-400 mt-3">
                    Maxing out every session instead would fit only ~
                    {formatSessions(cap.maxed_sessions_per_week)} per week.
                  </div>
                )}
              </div>
              <CapacityBars weeks={cap.weeks} />
            </div>
            <div className="text-[10px] text-zinc-400 mt-4 leading-relaxed">
              Each bar is one weekly window filled toward its 100% cap; each
              segment is one 5h session. A rose line marks weeks that hit the
              limit. Faded bars are partial (collection began mid-week) or the
              current week still in progress.
            </div>
          </>
        ) : (
          <Empty hint="Need at least 3 completed work sessions (each using 3%+ of the weekly limit). Keep the daemon running and this fills in as you work." />
        )}
      </div>
    </Section>
  );
}

function CapacityBars({ weeks }: { weeks: WeekCapacity[] }) {
  if (!weeks.length) return null;
  const TRACK = 150; // px; the full track height represents 100% of the week
  return (
    <div className="overflow-x-auto">
      <div className="flex items-end gap-2 pt-2 pb-1">
        {weeks.map((wk, wi) => {
          const faded = wk.partial || wk.in_progress;
          const count = wk.sessions.length;
          return (
            <div key={wi} className="flex flex-col items-center gap-1 shrink-0">
              <div
                className="relative w-9 rounded bg-zinc-100 dark:bg-zinc-800 overflow-hidden"
                style={{ height: TRACK }}
                title={
                  `Week of ${fmtDate(wk.start_unix_ms)}: ${Math.round(
                    wk.total_week_pct,
                  )}% of the weekly limit across ${count} session${count === 1 ? '' : 's'}` +
                  (wk.hit_cap ? ' · hit the cap' : '') +
                  (wk.in_progress
                    ? ' · in progress'
                    : wk.partial
                      ? ' · partial'
                      : '')
                }
              >
                <div className="absolute inset-x-0 bottom-0 flex flex-col-reverse">
                  {wk.sessions.map((sess, si) => (
                    <div
                      key={si}
                      style={{
                        height: Math.max(1, (sess.week_pct / 100) * TRACK),
                        backgroundColor: `rgba(14,165,233,${
                          faded ? 0.28 : si % 2 ? 0.55 : 0.8
                        })`,
                        boxShadow: 'inset 0 1px 0 rgba(255,255,255,0.35)',
                      }}
                      title={`${fmtDate(sess.start_unix_ms)}: ${fmtNumber(
                        sess.week_pct,
                      )}% of the week`}
                    />
                  ))}
                </div>
                {wk.hit_cap && (
                  <div
                    className="absolute inset-x-0 top-0 h-[3px] bg-rose-500"
                    title="Hit the weekly cap"
                  />
                )}
              </div>
              <div className="text-[10px] text-zinc-500 tabular-nums whitespace-nowrap">
                {fmtDate(wk.start_unix_ms)}
              </div>
              <div className="text-[10px] text-zinc-400 tabular-nums">
                {Math.round(wk.total_week_pct)}%
              </div>
            </div>
          );
        })}
      </div>
    </div>
  );
}

function formatSessions(n: number): string {
  return n >= 10 ? String(Math.round(n)) : n.toFixed(1);
}

function formatCadence(days: number): string {
  if (days >= 1) return `${days.toFixed(1)} days`;
  const hours = Math.round(days * 24);
  return `${hours} hour${hours === 1 ? '' : 's'}`;
}

function fmtDate(ms: number): string {
  return new Date(ms).toLocaleDateString(undefined, {
    month: 'short',
    day: 'numeric',
  });
}

function UsageCard({
  label,
  ts,
  values,
  color,
  resetTS,
}: {
  label: string;
  ts: number[];
  values: (number | null)[];
  color: string;
  resetTS?: number[];
}) {
  const hasData = ts.length > 1 && values.some((v) => v !== null);
  const vLines = resetTS?.map((x) => ({
    x,
    color: 'rgba(113,113,122,0.45)',
    dash: [2, 4],
  }));
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
          vLines={vLines}
        />
      ) : (
        <Empty />
      )}
    </div>
  );
}

function BurnCard({
  label,
  points,
  pick,
  color,
  resetTS,
  sustainable,
  sustainableHint,
}: {
  label: string;
  points: BurnPoint[];
  pick: (p: BurnPoint) => number | undefined;
  color: string;
  resetTS?: number[];
  sustainable: number;
  sustainableHint: string;
}) {
  const data = useMemo(() => {
    const ts = points.map((p) => p.ts_unix_ms / 1000);
    const values = points.map((p) => pick(p) ?? null);
    const measured = values.filter((v): v is number => v !== null);
    return {
      ts,
      values,
      peak: measured.length ? Math.max(...measured) : null,
    };
  }, [points, pick]);

  const hasData = data.values.some((v) => v !== null);

  return (
    <div className="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
      <div className="flex items-baseline justify-between gap-3 mb-4">
        <div className="flex flex-col">
          <span className="text-xs uppercase tracking-wider text-zinc-500">{label}</span>
          <span className="text-[10px] text-zinc-500 mt-0.5">
            Peak in window
          </span>
        </div>
        {data.peak != null && (
          <div className="text-right">
            <span className="text-4xl font-semibold tabular-nums text-zinc-800 dark:text-zinc-100">
              {data.peak.toFixed(data.peak < 10 ? 1 : 0)}
            </span>
            <span className="text-sm text-zinc-500 ml-1">%/h</span>
          </div>
        )}
      </div>
      {hasData ? (
        <Spark
          ts={data.ts}
          height={200}
          ySuffix="%/h"
          series={[{ label: '%/hour', values: data.values, color, width: 1.5 }]}
          hLines={[
            {
              value: sustainable,
              color: 'rgba(113,113,122,0.55)',
              dash: [2, 4],
              label: `${sustainable < 1 ? sustainable.toFixed(2) : sustainable}%/h exhausts the window`,
            },
          ]}
          vLines={resetTS?.map((x) => ({
            x,
            color: 'rgba(113,113,122,0.45)',
            dash: [2, 4],
          }))}
        />
      ) : (
        <Empty hint="Need observations spanning at least 10 minutes inside one limit window." />
      )}
      <div className="text-[10px] text-zinc-500 mt-2">{sustainableHint}</div>
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
  resetTS,
}: {
  label: string;
  points: CalibrationPoint[];
  medianCW?: number;
  medianN: number;
  color: string;
  colorFaint: string;
  resetTS?: number[];
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
          vLines={resetTS?.map((x) => ({
            x,
            color: 'rgba(113,113,122,0.45)',
            dash: [2, 4],
          }))}
        />
      ) : (
        <Empty hint="Need 2+ adjacent non-saturated observations in this bucket before this fills in." />
      )}
    </div>
  );
}

/** Insert a synthetic null between any pair of points where the second is
 *  flagged as a reset. uPlot stops drawing across nulls (spanGaps=false),
 *  so the chart no longer connects the pre-reset peak to the post-reset
 *  near-zero with a misleading downward diagonal. */
function breakAtResets(
  obs: ObservationPoint[],
  pickValue: (o: ObservationPoint) => number | null | undefined,
  isReset: (o: ObservationPoint) => boolean,
): { ts: number[]; values: (number | null)[] } {
  const ts: number[] = [];
  const values: (number | null)[] = [];
  for (let i = 0; i < obs.length; i++) {
    const o = obs[i];
    if (i > 0 && isReset(o)) {
      const prevTS = obs[i - 1].ts_unix_ms / 1000;
      const thisTS = o.ts_unix_ms / 1000;
      ts.push((prevTS + thisTS) / 2);
      values.push(null);
    }
    ts.push(o.ts_unix_ms / 1000);
    const v = pickValue(o);
    values.push(v == null ? null : v);
  }
  return { ts, values };
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
