import { Combine, Snowflake, Flame } from 'lucide-react';
import { Link } from 'react-router-dom';
import { useApi } from '../hooks/useApi';
import { fmtAbs, fmtNumber, fmtRel } from '../lib/format';

type CompactionItem = {
  id: number;
  session_uuid: string;
  project: string;
  ts: string;
  ts_unix_ms: number;
  prefix_tokens_est: number;
  summary_tokens_est: number;
  gap_to_prev_s?: number;
  cache_state: string;
  confirmed: boolean;
  confirm_reason?: string;
};

type CompactionsResponse = {
  items: CompactionItem[];
  count: number;
  by_cache_state: Record<string, number>;
  cold_count: number;
  warm_count: number;
  unknown_count: number;
  wasted_tokens: number;
};

export default function Compactions() {
  const { data, error, loading } = useApi<CompactionsResponse>('/compactions', 60_000);

  return (
    <div>
      <div className="flex items-center gap-2 mb-2">
        <Combine className="size-5 text-zinc-600 dark:text-zinc-400" />
        <h1 className="text-2xl font-semibold tracking-tight">Compactions</h1>
      </div>
      <p className="text-zinc-600 dark:text-zinc-400 max-w-2xl mb-6">
        Every confirmed <code className="font-mono text-xs">/compact</code> Bloodhound has
        seen. Compacting on a warm cache is essentially free; compacting cold
        re-uploads the entire prefix at full token cost.
      </p>

      {error && <div className="text-sm text-red-600 dark:text-red-400 mb-4">{error.message}</div>}
      {loading && !data && <div className="text-sm text-zinc-500">Loading…</div>}

      {data && (
        <>
          <div className="grid grid-cols-2 md:grid-cols-4 gap-3 mb-6 max-w-3xl">
            <Stat title="Total" value={fmtNumber(data.count)} />
            <Stat title="Warm" value={fmtNumber(data.warm_count)} icon={<Flame className="size-3.5 text-amber-500" />} />
            <Stat title="Cold" value={fmtNumber(data.cold_count)} icon={<Snowflake className="size-3.5 text-sky-500" />} accent={data.cold_count > 0 ? 'text-red-500' : undefined} />
            <Stat
              title="Cold prefix tokens"
              value={fmtNumber(data.wasted_tokens)}
              accent="text-red-500"
              hint="Tokens re-cached because the cache had expired before compaction"
            />
          </div>

          <div className="overflow-x-auto rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 max-w-5xl">
            <table className="w-full text-sm">
              <thead className="text-left text-xs uppercase tracking-wider text-zinc-500 border-b border-zinc-200 dark:border-zinc-800">
                <tr>
                  <Th>When</Th>
                  <Th>Project / Session</Th>
                  <Th className="text-right">Prefix</Th>
                  <Th className="text-right">Summary</Th>
                  <Th>Cache</Th>
                  <Th>Gap to prev</Th>
                </tr>
              </thead>
              <tbody>
                {data.items.map((c) => (
                  <tr
                    key={c.id}
                    className="border-b border-zinc-100 dark:border-zinc-800/60 hover:bg-zinc-50 dark:hover:bg-zinc-800/40"
                  >
                    <td className="px-3 py-2 text-zinc-500" title={fmtAbs(c.ts)}>
                      {fmtRel((Date.now() - c.ts_unix_ms) / 1000)}
                    </td>
                    <td className="px-3 py-2 font-mono text-xs">
                      <div className="truncate max-w-xs text-zinc-700 dark:text-zinc-300" title={c.project}>
                        {stripProject(c.project)}
                      </div>
                      <Link
                        to={`/sessions/${c.session_uuid}`}
                        className="text-[10px] text-zinc-500 hover:text-zinc-900 dark:hover:text-zinc-100"
                      >
                        {c.session_uuid.slice(0, 8)}
                      </Link>
                    </td>
                    <td className="px-3 py-2 text-right tabular-nums text-rose-500">{fmtNumber(c.prefix_tokens_est)}</td>
                    <td className="px-3 py-2 text-right tabular-nums">{fmtNumber(c.summary_tokens_est)}</td>
                    <td className="px-3 py-2"><CacheBadge state={c.cache_state} /></td>
                    <td className="px-3 py-2 text-zinc-500 text-xs tabular-nums">
                      {c.gap_to_prev_s !== undefined ? fmtGapS(c.gap_to_prev_s) : '—'}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
            {data.items.length === 0 && (
              <div className="text-center text-sm text-zinc-500 py-8">No confirmed compactions yet.</div>
            )}
          </div>
        </>
      )}
    </div>
  );
}

function Stat({
  title,
  value,
  icon,
  accent,
  hint,
}: {
  title: string;
  value: string;
  icon?: React.ReactNode;
  accent?: string;
  hint?: string;
}) {
  return (
    <div className="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-3" title={hint}>
      <div className="text-[10px] uppercase tracking-wider text-zinc-500 flex items-center gap-1">
        {icon}
        {title}
      </div>
      <div className={`text-xl font-semibold tabular-nums mt-0.5 ${accent || ''}`}>{value}</div>
    </div>
  );
}

function Th({ children, className }: { children: React.ReactNode; className?: string }) {
  return <th className={`px-3 py-2 font-medium ${className || ''}`}>{children}</th>;
}

function CacheBadge({ state }: { state: string }) {
  const styles: Record<string, string> = {
    cold: 'bg-sky-500/15 text-sky-700 dark:text-sky-300',
    warm_5m: 'bg-amber-500/15 text-amber-700 dark:text-amber-300',
    warm_1h: 'bg-emerald-500/15 text-emerald-700 dark:text-emerald-300',
    unknown: 'bg-zinc-500/15 text-zinc-600 dark:text-zinc-400',
  };
  const label =
    state === 'cold'
      ? 'cold'
      : state === 'warm_5m'
      ? 'warm 5m'
      : state === 'warm_1h'
      ? 'warm 1h'
      : 'unknown';
  return (
    <span className={`inline-block rounded px-1.5 py-0.5 text-[11px] font-mono ${styles[state] ?? styles.unknown}`}>
      {label}
    </span>
  );
}

function fmtGapS(s: number): string {
  if (s < 60) return `${Math.round(s)}s`;
  if (s < 3600) return `${Math.round(s / 60)}m`;
  return `${(s / 3600).toFixed(1)}h`;
}

function stripProject(p: string): string {
  return p.replace(/^-?home-[^-]+-dev-/, '').replace(/^-+/, '');
}
