import { useMemo } from 'react';
import { Link, useParams } from 'react-router-dom';
import { ChevronLeft, Files } from 'lucide-react';
import { useApi } from '../hooks/useApi';
import { fmtAbs, fmtNumber, fmtRel } from '../lib/format';

type TurnItem = {
  turn_idx: number;
  ts: string;
  ts_unix_ms: number;
  model: string;
  input_tokens: number;
  output_tokens: number;
  cache_read: number;
  cache_create_5m: number;
  cache_create_1h: number;
  gap_s: number;
  classification: string;
  post_compact: boolean;
};

type CompactionItem = {
  id: number;
  ts: string;
  ts_unix_ms: number;
  prefix_tokens_est: number;
  summary_tokens_est: number;
  gap_to_prev_s?: number;
  cache_state: string;
};

type SessionDetail = {
  session_uuid: string;
  project: string;
  first_ts: string;
  last_ts: string;
  turn_count: number;
  raw_tokens: number;
  output_tokens: number;
  peak_5h_raw_tokens: number;
  models: string;
  cache_ttl: string;
  idle_miss_count: number;
  rotation_count: number;
  restructure_count: number;
  compaction_count: number;
  cold_compaction_count: number;
  turns: TurnItem[];
  compactions: CompactionItem[];
  attribution: SessionAttributionDetail;
};

type SessionAttributionDetail = {
  week_pct: number;
  five_h_pct: number;
  five_h_peak_pct: number;
  measured_pct: number;
  estimated_pct: number;
  windows_5h?: number;
  windows_week?: number;
  week: SessionWindowSlice[];
  five_h: SessionWindowSlice[];
};

type SessionWindowSlice = {
  window_start_unix_ms: number;
  window_end_unix_ms: number;
  inferred?: boolean;
  partial?: boolean;
  in_progress?: boolean;
  pct: number;
  measured_pct: number;
  estimated_pct: number;
  window_pct: number;
  cw_tokens: number;
  raw_tokens: number;
  turn_count: number;
};

type TimelineEntry =
  | { kind: 'turn'; t: TurnItem }
  | { kind: 'compaction'; t: CompactionItem };

export default function SessionDetail() {
  const { uuid } = useParams<{ uuid: string }>();
  const { data, error, loading } = useApi<SessionDetail>(`/sessions/${uuid}`);

  const timeline = useMemo<TimelineEntry[]>(() => {
    if (!data) return [];
    const t: TimelineEntry[] = [
      ...data.turns.map<TimelineEntry>((x) => ({ kind: 'turn', t: x })),
      ...data.compactions.map<TimelineEntry>((x) => ({ kind: 'compaction', t: x })),
    ];
    t.sort((a, b) => entryMS(a) - entryMS(b));
    return t;
  }, [data]);

  let cumulative = 0;

  return (
    <div>
      <div className="flex items-center gap-2 mb-2">
        <Link to="/sessions" className="text-zinc-500 hover:text-zinc-900 dark:hover:text-zinc-100">
          <ChevronLeft className="size-5" />
        </Link>
        <Files className="size-5 text-zinc-600 dark:text-zinc-400" />
        <h1 className="text-2xl font-semibold tracking-tight">Session detail</h1>
      </div>
      {error && <div className="text-sm text-red-600 dark:text-red-400 mb-4">{error.message}</div>}
      {loading && !data && <div className="text-sm text-zinc-500">Loading…</div>}

      {data && (
        <>
          <div className="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-4 mb-6 max-w-5xl">
            <div className="font-mono text-xs text-zinc-500 mb-1">{data.session_uuid}</div>
            <div className="text-sm font-mono text-zinc-700 dark:text-zinc-300 truncate">{data.project}</div>
            <div className="grid grid-cols-2 md:grid-cols-4 gap-3 mt-4 text-sm">
              <KV k="First" v={fmtAbs(data.first_ts)} />
              <KV k="Last" v={`${fmtAbs(data.last_ts)} (${fmtRel((Date.now() - new Date(data.last_ts).getTime()) / 1000)})`} />
              <KV k="Turns" v={data.turn_count.toLocaleString()} />
              <KV k="Models" v={data.models} mono />
              <KV k="Raw tokens" v={fmtNumber(data.raw_tokens)} />
              <KV k="Output" v={fmtNumber(data.output_tokens)} />
              <KV k="Peak 5h" v={fmtNumber(data.peak_5h_raw_tokens)} accent="text-rose-500" />
              <KV k="Cache TTL" v={data.cache_ttl} mono />
              <KV
                k="% of week"
                v={fmtLimitPct(data.attribution.week_pct)}
                accent="text-sky-600 dark:text-sky-400"
              />
              <KV
                k="% of 5h (peak)"
                v={fmtLimitPct(data.attribution.five_h_peak_pct)}
                accent="text-sky-600 dark:text-sky-400"
              />
            </div>
            <div className="grid grid-cols-2 md:grid-cols-4 gap-3 mt-3 text-sm">
              <KV k="Idle miss" v={data.idle_miss_count.toLocaleString()} accent={data.idle_miss_count > 0 ? 'text-red-500' : undefined} />
              <KV k="Rotation" v={data.rotation_count.toLocaleString()} accent={data.rotation_count > 0 ? 'text-amber-500' : undefined} />
              <KV k="Restructure" v={data.restructure_count.toLocaleString()} accent={data.restructure_count > 0 ? 'text-purple-500' : undefined} />
              <KV
                k="Compactions"
                v={
                  data.compaction_count > 0
                    ? `${data.compaction_count}${data.cold_compaction_count > 0 ? ` (${data.cold_compaction_count} cold)` : ''}`
                    : '0'
                }
                accent={data.cold_compaction_count > 0 ? 'text-sky-500' : undefined}
              />
            </div>
          </div>

          <LimitCost attr={data.attribution} />

          <div className="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 max-w-5xl overflow-x-auto">
            <table className="w-full text-sm">
              <thead className="text-left text-xs uppercase tracking-wider text-zinc-500 border-b border-zinc-200 dark:border-zinc-800">
                <tr>
                  <Th className="text-right">#</Th>
                  <Th>When</Th>
                  <Th>Class</Th>
                  <Th className="text-right">In</Th>
                  <Th className="text-right">Out</Th>
                  <Th className="text-right">CR</Th>
                  <Th className="text-right">CW (5m+1h)</Th>
                  <Th className="text-right">Cumulative</Th>
                  <Th className="text-right">Gap</Th>
                </tr>
              </thead>
              <tbody>
                {timeline.map((e) => {
                  if (e.kind === 'compaction') {
                    return (
                      <tr key={`c-${e.t.id}`} className="bg-sky-50 dark:bg-sky-950/30 border-b border-sky-200 dark:border-sky-900">
                        <td className="px-3 py-2 text-right text-sky-700 dark:text-sky-300">⇒</td>
                        <td className="px-3 py-2 text-zinc-500 text-xs" title={fmtAbs(e.t.ts)}>{fmtAbs(e.t.ts)}</td>
                        <td className="px-3 py-2" colSpan={6}>
                          <span className="font-medium text-sky-700 dark:text-sky-300">/compact</span>{' '}
                          <span className="text-xs text-zinc-500">
                            prefix {fmtNumber(e.t.prefix_tokens_est)} → summary {fmtNumber(e.t.summary_tokens_est)} ·{' '}
                            <span className="font-mono">{e.t.cache_state}</span>
                          </span>
                        </td>
                        <td className="px-3 py-2 text-right text-zinc-500 text-xs tabular-nums">
                          {e.t.gap_to_prev_s !== undefined ? fmtGap(e.t.gap_to_prev_s) : '—'}
                        </td>
                      </tr>
                    );
                  }
                  const t = e.t;
                  const cw = t.cache_create_5m + t.cache_create_1h;
                  const raw = t.input_tokens + t.output_tokens + t.cache_read + cw;
                  cumulative += raw;
                  const classColor: Record<string, string> = {
                    idle_miss: 'text-red-500',
                    rotation: 'text-amber-500',
                    restructure: 'text-purple-500',
                    normal: 'text-zinc-400',
                  };
                  return (
                    <tr
                      key={`t-${t.turn_idx}`}
                      className={`border-b border-zinc-100 dark:border-zinc-800/60 ${t.post_compact ? 'bg-zinc-50 dark:bg-zinc-800/30' : ''}`}
                    >
                      <td className="px-3 py-1.5 text-right tabular-nums text-zinc-500">{t.turn_idx}</td>
                      <td className="px-3 py-1.5 text-zinc-500 text-xs" title={fmtAbs(t.ts)}>{fmtAbs(t.ts)}</td>
                      <td className={`px-3 py-1.5 text-xs ${classColor[t.classification] ?? 'text-zinc-500'}`}>
                        {t.classification === 'normal' ? '' : t.classification}
                      </td>
                      <td className="px-3 py-1.5 text-right tabular-nums">{fmtNumber(t.input_tokens)}</td>
                      <td className="px-3 py-1.5 text-right tabular-nums">{fmtNumber(t.output_tokens)}</td>
                      <td className="px-3 py-1.5 text-right tabular-nums text-emerald-600 dark:text-emerald-400">{fmtNumber(t.cache_read)}</td>
                      <td className="px-3 py-1.5 text-right tabular-nums text-amber-600 dark:text-amber-400">{fmtNumber(cw)}</td>
                      <td className="px-3 py-1.5 text-right tabular-nums text-zinc-500">{fmtNumber(cumulative)}</td>
                      <td className="px-3 py-1.5 text-right tabular-nums text-zinc-500 text-xs">{fmtGap(t.gap_s)}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
            {timeline.length === 0 && (
              <div className="text-center text-sm text-zinc-500 py-8">No turns recorded for this session.</div>
            )}
          </div>
        </>
      )}
    </div>
  );
}

/** What this one conversation cost against each limit meter, window by
 *  window. The weekly view is the headline: weekly windows are long enough
 *  that most sessions sit inside exactly one, while the 5h view shows how
 *  the work spread across the shorter meter. */
function LimitCost({ attr }: { attr: SessionAttributionDetail }) {
  const nothing = attr.week.length === 0 && attr.five_h.length === 0;
  return (
    <div className="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-4 mb-6 max-w-5xl">
      <div className="flex items-baseline gap-2 mb-1">
        <h2 className="text-sm font-semibold tracking-tight">Limit cost</h2>
        <Link to="/attribution" className="text-[11px] text-zinc-500 hover:text-rose-500">
          see all sessions →
        </Link>
      </div>
      {nothing ? (
        <p className="text-xs text-zinc-500">
          Not attributed yet: the aggregator picks this up on its next pass
          (every 15 minutes by default).
        </p>
      ) : (
        <>
          <p className="text-xs text-zinc-500 mb-4">
            This session took{' '}
            <span className="font-medium text-zinc-700 dark:text-zinc-300">
              {fmtLimitPct(attr.week_pct)}
            </span>{' '}
            of the weekly limit
            {attr.windows_5h && attr.windows_5h > 1 ? (
              <>
                {' '}and spread across {attr.windows_5h} five-hour windows, the
                worst taking{' '}
                <span className="font-medium text-zinc-700 dark:text-zinc-300">
                  {fmtLimitPct(attr.five_h_peak_pct)}
                </span>
                .
              </>
            ) : (
              <>
                {' '}and{' '}
                <span className="font-medium text-zinc-700 dark:text-zinc-300">
                  {fmtLimitPct(attr.five_h_peak_pct)}
                </span>{' '}
                of its 5-hour window.
              </>
            )}
            {attr.estimated_pct > 0.005 && (
              <> {fmtLimitPct(attr.estimated_pct)} of the weekly figure is estimated.</>
            )}
          </p>
          <div className="grid gap-5 md:grid-cols-2">
            <WindowBreakdown title="Weekly windows" slices={attr.week} />
            <WindowBreakdown title="5-hour windows" slices={attr.five_h} />
          </div>
        </>
      )}
    </div>
  );
}

/** One row per window: this session's share, drawn against everything else
 *  attributed to the same window so the bar reads as "my slice of that". */
function WindowBreakdown({ title, slices }: { title: string; slices: SessionWindowSlice[] }) {
  if (slices.length === 0) return null;
  return (
    <div>
      <div className="text-[11px] uppercase tracking-wider text-zinc-500 mb-2">{title}</div>
      <div className="space-y-1.5">
        {slices.map((s) => {
          const share = s.window_pct > 0 ? Math.min(1, s.pct / s.window_pct) : 0;
          return (
            <div key={s.window_start_unix_ms} className="flex items-center gap-2 text-xs">
              <div className="w-16 shrink-0 text-zinc-500 tabular-nums">
                {fmtWindowDate(s.window_start_unix_ms)}
              </div>
              <div
                className="flex-1 h-2 rounded-full bg-zinc-100 dark:bg-zinc-800 overflow-hidden"
                title={`${fmtLimitPct(s.pct)} of this window, which used ${fmtLimitPct(s.window_pct)} in total · ${s.turn_count.toLocaleString()} turns`}
              >
                <div
                  className="h-full rounded-full bg-sky-500/80"
                  style={{ width: `${share * 100}%` }}
                />
              </div>
              <div className="w-12 shrink-0 text-right tabular-nums text-zinc-600 dark:text-zinc-400">
                {fmtLimitPct(s.pct)}
              </div>
              {s.inferred && (
                <span className="text-[10px] text-zinc-400 shrink-0" title="estimated: no /usage readings covered this window">
                  est
                </span>
              )}
            </div>
          );
        })}
      </div>
    </div>
  );
}

function fmtLimitPct(n: number): string {
  if (!n) return '0%';
  if (n < 0.1) return `${n.toFixed(2)}%`;
  if (n < 10) return `${n.toFixed(1)}%`;
  return `${Math.round(n)}%`;
}

function fmtWindowDate(ms: number): string {
  return new Date(ms).toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
}

function entryMS(e: TimelineEntry): number {
  return e.kind === 'turn' ? e.t.ts_unix_ms : e.t.ts_unix_ms;
}

function Th({ children, className }: { children: React.ReactNode; className?: string }) {
  return <th className={`px-3 py-2 font-medium ${className || ''}`}>{children}</th>;
}

function KV({ k, v, mono, accent }: { k: string; v: string | number; mono?: boolean; accent?: string }) {
  return (
    <div>
      <div className="text-[10px] uppercase tracking-wider text-zinc-500">{k}</div>
      <div className={`mt-0.5 ${mono ? 'font-mono text-xs' : ''} ${accent || ''}`}>{v}</div>
    </div>
  );
}

function fmtGap(s: number): string {
  if (s <= 0) return '—';
  if (s < 60) return `${Math.round(s)}s`;
  if (s < 3600) return `${Math.round(s / 60)}m`;
  return `${(s / 3600).toFixed(1)}h`;
}
