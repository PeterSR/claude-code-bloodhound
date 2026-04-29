import { Droplet, AlertTriangle } from 'lucide-react';
import { Link } from 'react-router-dom';
import { useApi } from '../hooks/useApi';
import { fmtNumber } from '../lib/format';

type SessionLeakRow = {
  session_uuid: string;
  project: string;
  idle_miss_count: number;
  rotation_count: number;
  restructure_count: number;
  cold_compaction_count: number;
  avoidable_score: number;
  raw_tokens: number;
};

type LeaksResponse = {
  idle_miss_tokens: number;
  idle_miss_turn_count: number;
  rotation_tokens: number;
  rotation_turn_count: number;
  restructure_tokens: number;
  restructure_turn_count: number;
  cold_compaction_tokens: number;
  cold_compaction_count: number;
  total_raw_tokens: number;
  total_cw_tokens: number;
  top_offenders: SessionLeakRow[];
};

export default function Leaks() {
  const { data, error, loading } = useApi<LeaksResponse>('/leaks', 60_000);

  return (
    <div>
      <div className="flex items-center gap-2 mb-2">
        <Droplet className="size-5 text-rose-500" />
        <h1 className="text-2xl font-semibold tracking-tight">Leaks</h1>
      </div>
      <p className="text-zinc-600 dark:text-zinc-400 max-w-2xl mb-6">
        Avoidable spend, ranked. Each category is something you can plausibly
        change — reply faster (idle misses), use longer-lived breakpoints
        (rotations), compact while warm (cold compactions).
      </p>

      {error && <div className="text-sm text-red-600 dark:text-red-400 mb-4">{error.message}</div>}
      {loading && !data && <div className="text-sm text-zinc-500">Loading…</div>}

      {data && (
        <>
          <div className="grid grid-cols-2 lg:grid-cols-4 gap-3 mb-6 max-w-4xl">
            <LeakCard
              title="Idle misses"
              tokens={data.idle_miss_tokens}
              count={data.idle_miss_turn_count}
              hint="Cache expired during idle time; the prefix had to be re-cached on the next turn."
              ratioOf={data.total_raw_tokens}
            />
            <LeakCard
              title="Rotations"
              tokens={data.rotation_tokens}
              count={data.rotation_turn_count}
              hint="Claude Code rotated cache breakpoints (the API caps at 4)."
              ratioOf={data.total_raw_tokens}
            />
            <LeakCard
              title="Restructures"
              tokens={data.restructure_tokens}
              count={data.restructure_turn_count}
              hint="Prefix shifted within a turn (e.g. an image upload), re-caching part of it."
              ratioOf={data.total_raw_tokens}
            />
            <LeakCard
              title="Cold compactions"
              tokens={data.cold_compaction_tokens}
              count={data.cold_compaction_count}
              hint="Prefix tokens re-uploaded because cache had expired before /compact."
              ratioOf={data.total_raw_tokens}
            />
          </div>

          <div className="text-xs text-zinc-500 mb-3 max-w-3xl">
            Total raw tokens across all turns:{' '}
            <span className="font-mono text-zinc-700 dark:text-zinc-300">{fmtNumber(data.total_raw_tokens)}</span>{' '}
            · Cost-weighted:{' '}
            <span className="font-mono text-zinc-700 dark:text-zinc-300">{fmtNumber(data.total_cw_tokens)}</span>.
          </div>

          <div className="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 max-w-5xl">
            <div className="p-4 text-sm font-medium text-zinc-700 dark:text-zinc-300 border-b border-zinc-200 dark:border-zinc-800">
              Top offenders
              <span className="ml-2 text-xs font-normal text-zinc-500">
                Sessions ranked by leak token count
              </span>
            </div>
            <div className="overflow-x-auto">
              <table className="w-full text-sm">
                <thead className="text-left text-xs uppercase tracking-wider text-zinc-500 border-b border-zinc-200 dark:border-zinc-800">
                  <tr>
                    <Th>Project / Session</Th>
                    <Th className="text-right">Idle</Th>
                    <Th className="text-right">Rot</Th>
                    <Th className="text-right">Restruct</Th>
                    <Th className="text-right">Cold compact</Th>
                    <Th className="text-right">Leak tokens</Th>
                    <Th className="text-right">% of session</Th>
                  </tr>
                </thead>
                <tbody>
                  {data.top_offenders.map((r) => {
                    const pct = r.raw_tokens > 0 ? (r.avoidable_score / r.raw_tokens) * 100 : 0;
                    return (
                      <tr
                        key={r.session_uuid}
                        className="border-b border-zinc-100 dark:border-zinc-800/60 hover:bg-zinc-50 dark:hover:bg-zinc-800/40"
                      >
                        <td className="px-3 py-2 font-mono text-xs">
                          <div className="truncate max-w-xs text-zinc-700 dark:text-zinc-300" title={r.project}>
                            {stripProject(r.project)}
                          </div>
                          <Link
                            to={`/sessions/${r.session_uuid}`}
                            className="text-[10px] text-zinc-500 hover:text-zinc-900 dark:hover:text-zinc-100"
                          >
                            {r.session_uuid.slice(0, 8)}
                          </Link>
                        </td>
                        <td className="px-3 py-2 text-right tabular-nums text-red-500">
                          {r.idle_miss_count > 0 ? r.idle_miss_count : <span className="text-zinc-400">0</span>}
                        </td>
                        <td className="px-3 py-2 text-right tabular-nums text-amber-500">
                          {r.rotation_count > 0 ? r.rotation_count : <span className="text-zinc-400">0</span>}
                        </td>
                        <td className="px-3 py-2 text-right tabular-nums text-purple-500">
                          {r.restructure_count > 0 ? r.restructure_count : <span className="text-zinc-400">0</span>}
                        </td>
                        <td className="px-3 py-2 text-right tabular-nums">
                          {r.cold_compaction_count > 0 ? <span className="text-sky-500">{r.cold_compaction_count}</span> : <span className="text-zinc-400">0</span>}
                        </td>
                        <td className="px-3 py-2 text-right tabular-nums font-medium">{fmtNumber(r.avoidable_score)}</td>
                        <td className="px-3 py-2 text-right tabular-nums text-zinc-500">{pct.toFixed(0)}%</td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
              {data.top_offenders.length === 0 && (
                <div className="text-center text-sm text-zinc-500 py-8 flex flex-col items-center gap-2">
                  <AlertTriangle className="size-5 text-emerald-500" />
                  <span>No leaks detected. Either you're already efficient or the dataset is too small.</span>
                </div>
              )}
            </div>
          </div>
        </>
      )}
    </div>
  );
}

function LeakCard({
  title,
  tokens,
  count,
  hint,
  ratioOf,
}: {
  title: string;
  tokens: number;
  count: number;
  hint: string;
  ratioOf: number;
}) {
  const pct = ratioOf > 0 ? (tokens / ratioOf) * 100 : 0;
  return (
    <div className="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-4" title={hint}>
      <div className="text-xs uppercase tracking-wider text-zinc-500">{title}</div>
      <div className="mt-1 flex items-baseline justify-between gap-2">
        <div className="text-xl font-semibold tabular-nums">{fmtNumber(tokens)}</div>
        <div className="text-xs text-zinc-500 tabular-nums">{pct.toFixed(1)}%</div>
      </div>
      <div className="text-xs text-zinc-500 mt-0.5">{count.toLocaleString()} turns</div>
    </div>
  );
}

function Th({ children, className }: { children: React.ReactNode; className?: string }) {
  return <th className={`px-3 py-2 font-medium ${className || ''}`}>{children}</th>;
}

function stripProject(p: string): string {
  return p.replace(/^-?home-[^-]+-dev-/, '').replace(/^-+/, '');
}
