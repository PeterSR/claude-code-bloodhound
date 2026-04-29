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
