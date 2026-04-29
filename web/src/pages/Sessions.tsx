import { useMemo, useState } from 'react';
import { Files, Search } from 'lucide-react';
import { Link } from 'react-router-dom';
import { useApi } from '../hooks/useApi';
import { fmtAbs, fmtNumber, fmtRel } from '../lib/format';

type Session = {
  session_uuid: string;
  project: string;
  first_ts: string;
  last_ts: string;
  turn_count: number;
  raw_tokens: number;
  output_tokens: number;
  peak_5h_raw_tokens: number;
  idle_miss_count: number;
  rotation_count: number;
  restructure_count: number;
  compaction_count: number;
  cold_compaction_count: number;
  cache_ttl: string;
  models: string;
};

type SessionsResponse = { sessions: Session[]; count: number };

type SortKey = 'last_ts' | 'turn_count' | 'raw_tokens' | 'peak_5h_raw_tokens' | 'compaction_count';

export default function Sessions() {
  const { data, loading, error } = useApi<SessionsResponse>('/sessions', 60_000);
  const [filter, setFilter] = useState('');
  const [sortKey, setSortKey] = useState<SortKey>('last_ts');
  const [sortDesc, setSortDesc] = useState(true);
  const [limit, setLimit] = useState(50);

  const rows = useMemo(() => {
    if (!data) return [];
    const q = filter.trim().toLowerCase();
    let out = data.sessions;
    if (q) {
      out = out.filter((s) =>
        (s.project + ' ' + s.session_uuid + ' ' + s.models).toLowerCase().includes(q),
      );
    }
    out = [...out].sort((a, b) => {
      const av = pick(a, sortKey);
      const bv = pick(b, sortKey);
      return sortDesc ? bv - av : av - bv;
    });
    return out;
  }, [data, filter, sortKey, sortDesc]);

  const visible = rows.slice(0, limit);

  return (
    <div>
      <div className="flex items-center gap-2 mb-2">
        <Files className="size-5 text-zinc-600 dark:text-zinc-400" />
        <h1 className="text-2xl font-semibold tracking-tight">Sessions</h1>
      </div>
      <p className="text-zinc-600 dark:text-zinc-400 max-w-2xl mb-4">
        Every Claude Code session this machine has on disk, with peak 5-hour
        burn and compaction breakdown. Click a header to sort.
      </p>

      <div className="flex flex-wrap gap-3 items-center mb-4">
        <div className="relative">
          <Search className="size-3.5 absolute left-2.5 top-1/2 -translate-y-1/2 text-zinc-400" />
          <input
            type="text"
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            placeholder="filter project / uuid / model"
            className="pl-8 pr-3 py-1.5 text-sm rounded-md border border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900 w-72 outline-none focus:border-zinc-400 dark:focus:border-zinc-500"
          />
        </div>
        <label className="text-xs text-zinc-500 flex items-center gap-1.5">
          show
          <input
            type="number"
            min={10}
            step={10}
            value={limit}
            onChange={(e) => setLimit(parseInt(e.target.value || '50', 10))}
            className="w-16 px-2 py-1 text-sm rounded-md border border-zinc-300 dark:border-zinc-700 bg-white dark:bg-zinc-900"
          />
          of {rows.length}
        </label>
      </div>

      {error && <div className="text-sm text-red-600 dark:text-red-400 mb-4">{error.message}</div>}
      {loading && !data && <div className="text-sm text-zinc-500">Loading…</div>}

      {data && (
        <div className="overflow-x-auto rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900">
          <table className="w-full text-sm">
            <thead className="text-left text-xs uppercase tracking-wider text-zinc-500 border-b border-zinc-200 dark:border-zinc-800">
              <tr>
                <Th>Last activity</Th>
                <Th>Project</Th>
                <Th sortable={onSort('turn_count')} active={sortKey === 'turn_count'} desc={sortDesc} className="text-right">Turns</Th>
                <Th sortable={onSort('raw_tokens')} active={sortKey === 'raw_tokens'} desc={sortDesc} className="text-right">Raw tokens</Th>
                <Th sortable={onSort('peak_5h_raw_tokens')} active={sortKey === 'peak_5h_raw_tokens'} desc={sortDesc} className="text-right">Peak 5h</Th>
                <Th className="text-right">Idle / Rot / Restruct</Th>
                <Th sortable={onSort('compaction_count')} active={sortKey === 'compaction_count'} desc={sortDesc} className="text-right">Compactions</Th>
                <Th>Cache</Th>
              </tr>
            </thead>
            <tbody>
              {visible.map((s) => (
                <tr key={s.session_uuid} className="border-b border-zinc-100 dark:border-zinc-800/60 hover:bg-zinc-50 dark:hover:bg-zinc-800/40">
                  <td className="px-3 py-2 text-zinc-500" title={fmtAbs(s.last_ts)}>
                    {fmtRel(secondsAgo(s.last_ts))}
                  </td>
                  <td className="px-3 py-2 font-mono text-xs text-zinc-700 dark:text-zinc-300">
                    <Link to={`/sessions/${s.session_uuid}`} className="hover:text-rose-500">
                      <div className="truncate max-w-xs" title={s.project}>{stripProject(s.project)}</div>
                      <div className="text-[10px] text-zinc-500">{s.session_uuid.slice(0, 8)}</div>
                    </Link>
                  </td>
                  <td className="px-3 py-2 text-right tabular-nums">{s.turn_count.toLocaleString()}</td>
                  <td className="px-3 py-2 text-right tabular-nums">{fmtNumber(s.raw_tokens)}</td>
                  <td className="px-3 py-2 text-right tabular-nums text-rose-500">{fmtNumber(s.peak_5h_raw_tokens)}</td>
                  <td className="px-3 py-2 text-right text-xs text-zinc-500 tabular-nums">
                    {warn(s.idle_miss_count, 'text-red-500')} / {warn(s.rotation_count, 'text-amber-500')} / {warn(s.restructure_count, 'text-purple-500')}
                  </td>
                  <td className="px-3 py-2 text-right tabular-nums">
                    {s.compaction_count > 0 ? (
                      <span title={s.cold_compaction_count > 0 ? `${s.cold_compaction_count} cold` : ''}>
                        {s.compaction_count}
                        {s.cold_compaction_count > 0 && (
                          <span className="text-red-500 ml-1">({s.cold_compaction_count}❄)</span>
                        )}
                      </span>
                    ) : (
                      <span className="text-zinc-500">0</span>
                    )}
                  </td>
                  <td className="px-3 py-2">
                    <CacheBadge ttl={s.cache_ttl} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {visible.length === 0 && (
            <div className="text-center text-sm text-zinc-500 py-8">No sessions match.</div>
          )}
        </div>
      )}
    </div>
  );

  function onSort(key: SortKey) {
    return () => {
      if (sortKey === key) setSortDesc((d) => !d);
      else {
        setSortKey(key);
        setSortDesc(true);
      }
    };
  }
}

function Th({
  children,
  sortable,
  active,
  desc,
  className,
}: {
  children: React.ReactNode;
  sortable?: () => void;
  active?: boolean;
  desc?: boolean;
  className?: string;
}) {
  return (
    <th
      onClick={sortable}
      className={[
        'px-3 py-2 font-medium select-none',
        sortable ? 'cursor-pointer hover:text-zinc-900 dark:hover:text-zinc-100' : '',
        active ? 'text-zinc-900 dark:text-zinc-100' : '',
        className || '',
      ].join(' ')}
    >
      {children}
      {active && <span className="ml-1 text-[10px]">{desc ? '▾' : '▴'}</span>}
    </th>
  );
}

function CacheBadge({ ttl }: { ttl: string }) {
  const colors: Record<string, string> = {
    '1h': 'bg-emerald-500/15 text-emerald-700 dark:text-emerald-300',
    '5m': 'bg-amber-500/15 text-amber-700 dark:text-amber-300',
    mix: 'bg-purple-500/15 text-purple-700 dark:text-purple-300',
    none: 'bg-zinc-500/15 text-zinc-600 dark:text-zinc-400',
  };
  const cls = colors[ttl] || colors.none;
  return <span className={`inline-block rounded px-1.5 py-0.5 text-[11px] font-mono ${cls}`}>{ttl}</span>;
}

function warn(n: number, color: string): React.ReactNode {
  if (n === 0) return <span className="text-zinc-400">0</span>;
  return <span className={color}>{n}</span>;
}

function stripProject(p: string): string {
  return p.replace(/^-?home-[^-]+-dev-/, '').replace(/^-+/, '');
}

function secondsAgo(iso: string): number {
  return (Date.now() - new Date(iso).getTime()) / 1000;
}

function pick(s: Session, key: SortKey): number {
  if (key === 'last_ts') return new Date(s.last_ts).getTime();
  return s[key];
}
