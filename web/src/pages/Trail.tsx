import { useMemo, useState } from 'react';
import { Link } from 'react-router-dom';
import {
  PawPrint,
  GitBranch,
  FolderGit2,
  Check,
  Circle,
  CircleDot,
  Coins,
} from 'lucide-react';
import ReloadButton from '../components/ReloadButton';
import { useApi } from '../hooks/useApi';
import { fmtNumber, fmtRel } from '../lib/format';

type RepoRef = { path: string; dirname: string; branch: string; role: string };
type Loop = {
  session_uuid: string;
  key: string;
  text: string;
  status: string;
  analyzer_status: string;
  user_status?: string;
  related_repo_path?: string;
  first_seen_unix_ms: number;
};
type Session = {
  session_uuid: string;
  project: string;
  cwd: string;
  headline: string;
  summary: string;
  updated_unix_ms: number;
  analyzed_runs: number;
  repos: RepoRef[];
  open_loops: Loop[];
};
type RepoGroup = {
  path: string;
  dirname: string;
  branch: string;
  common_dir?: string;
  sessions: { session_uuid: string; headline: string; role: string }[];
};
type Cost = {
  runs: number;
  cost_usd: number;
  input_tokens: number;
  output_tokens: number;
  cache_read_tokens: number;
  cache_create_tokens: number;
  cost_weighted_tokens: number;
  pct_of_week?: number;
};
type TrailResponse = {
  enabled: boolean;
  mode: string;
  interval_s: number;
  server_now_ms: number;
  sessions: Session[];
  repos: RepoGroup[];
  cost: Cost;
};

async function resolveLoop(session_uuid: string, loop_key: string, user_status: string) {
  await fetch('/api/trail/resolve', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ session_uuid, loop_key, user_status, note: '' }),
  });
}

export default function Trail() {
  const { data, error, loading, refreshing, refresh } = useApi<TrailResponse>('/trail', 30_000);
  const [view, setView] = useState<'session' | 'repo'>('session');

  return (
    <div>
      <div className="flex items-center justify-between mb-2">
        <div className="flex items-center gap-2">
          <PawPrint className="size-5 text-rose-500" />
          <h1 className="text-2xl font-semibold tracking-tight">Trail</h1>
        </div>
        <div className="flex items-center gap-3">
          <div className="flex rounded-md border border-zinc-300 dark:border-zinc-700 overflow-hidden text-xs">
            {(['session', 'repo'] as const).map((v) => (
              <button
                key={v}
                onClick={() => setView(v)}
                className={[
                  'px-3 py-1 capitalize',
                  view === v
                    ? 'bg-zinc-900 dark:bg-zinc-100 text-white dark:text-zinc-900'
                    : 'bg-white dark:bg-zinc-900 hover:bg-zinc-100 dark:hover:bg-zinc-800',
                ].join(' ')}
              >
                By {v}
              </button>
            ))}
          </div>
          <ReloadButton refreshing={refreshing} onClick={refresh} />
        </div>
      </div>
      <p className="text-zinc-600 dark:text-zinc-400 max-w-2xl mb-6">
        What's in flight across your active sessions — a passive overview of
        what each session is doing, which worktrees it touches, and its open
        loops. Summaries are generated on a schedule; nothing here acts on
        your work.
      </p>

      {error && (
        <div className="text-sm text-red-600 dark:text-red-400 mb-4">{error.message}</div>
      )}
      {loading && !data && <div className="text-sm text-zinc-500">Loading…</div>}

      {data && (
        <>
          {!data.enabled && (
            <div className="rounded-md border border-amber-300 dark:border-amber-700 bg-amber-50 dark:bg-amber-950/40 px-4 py-2 text-sm mb-5">
              Trail is <strong>off</strong>. Enable it under{' '}
              <Link to="/settings" className="underline hover:text-rose-500">Settings</Link>{' '}
              to start tracking active sessions. It drives <code className="font-mono text-xs">claude</code> to summarise, so it consumes usage.
            </div>
          )}

          <CostBar cost={data.cost} />

          {view === 'session' ? (
            <SessionView sessions={data.sessions} onResolve={refresh} />
          ) : (
            <RepoView repos={data.repos} />
          )}
        </>
      )}
    </div>
  );
}

function CostBar({ cost }: { cost: Cost }) {
  if (!cost || cost.runs === 0) return null;
  return (
    <div className="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 px-4 py-2.5 mb-5 flex flex-wrap items-center gap-x-6 gap-y-1 text-sm">
      <span className="inline-flex items-center gap-1.5 text-zinc-500">
        <Coins className="size-4" /> Trail has spent
      </span>
      <span>
        <strong className="tabular-nums">{fmtNumber(cost.cost_weighted_tokens)}</strong>
        <span className="text-zinc-500"> cost-weighted tokens</span>
      </span>
      {cost.pct_of_week ? (
        <span>
          ≈<strong className="tabular-nums">{cost.pct_of_week.toFixed(2)}%</strong>
          <span className="text-zinc-500"> of your week</span>
        </span>
      ) : null}
      {cost.cost_usd > 0 ? (
        <span className="tabular-nums">${cost.cost_usd.toFixed(3)}</span>
      ) : null}
      <span className="text-zinc-500 tabular-nums text-xs">
        {fmtNumber(cost.input_tokens)} in / {fmtNumber(cost.output_tokens)} out · {cost.runs} runs
      </span>
    </div>
  );
}

function SessionView({ sessions, onResolve }: { sessions: Session[]; onResolve: () => void }) {
  if (!sessions || sessions.length === 0) {
    return <Empty>No session briefs yet. Trail records a watermark the first time it sees a session and summarises new activity on the next cycle.</Empty>;
  }
  return (
    <div className="grid gap-4 grid-cols-1 xl:grid-cols-2">
      {sessions.map((s) => (
        <div key={s.session_uuid} className="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
          <div className="flex items-start justify-between gap-3 mb-1">
            <Link to={`/sessions/${s.session_uuid}`} className="font-medium hover:text-rose-500">
              {s.headline || '(no headline)'}
            </Link>
            <span className="text-[11px] text-zinc-500 shrink-0">{fmtRel((Date.now() - s.updated_unix_ms) / 1000)}</span>
          </div>
          <p className="text-sm text-zinc-600 dark:text-zinc-400 mb-3">{s.summary}</p>

          {s.repos && s.repos.length > 0 && (
            <div className="flex flex-wrap gap-1.5 mb-3">
              {s.repos.map((r) => (
                <RepoChip key={r.path} repo={r} />
              ))}
            </div>
          )}

          {s.open_loops && s.open_loops.length > 0 && (
            <div className="space-y-1.5">
              {s.open_loops.map((l) => (
                <LoopRow key={l.key} loop={l} onResolve={onResolve} />
              ))}
            </div>
          )}
        </div>
      ))}
    </div>
  );
}

function RepoChip({ repo }: { repo: RepoRef }) {
  const primary = repo.role === 'primary';
  return (
    <span
      className={[
        'inline-flex items-center gap-1 rounded px-1.5 py-0.5 text-[11px] font-mono',
        primary
          ? 'bg-rose-500/15 text-rose-700 dark:text-rose-300'
          : 'bg-zinc-500/10 text-zinc-600 dark:text-zinc-400',
      ].join(' ')}
      title={`${repo.path}${repo.role === 'incidental' ? ' (reached into)' : ''}`}
    >
      <FolderGit2 className="size-3" />
      {repo.dirname || repo.path}
      {repo.branch && (
        <span className="inline-flex items-center gap-0.5 opacity-80">
          <GitBranch className="size-2.5" />
          {repo.branch}
        </span>
      )}
    </span>
  );
}

function LoopRow({ loop, onResolve }: { loop: Loop; onResolve: () => void }) {
  const [busy, setBusy] = useState(false);
  const done = loop.status === 'done' || loop.status === 'dismissed';
  const cross = async () => {
    setBusy(true);
    await resolveLoop(loop.session_uuid, loop.key, done ? '' : 'done');
    setBusy(false);
    onResolve();
  };
  const statusColor =
    loop.status === 'waiting' ? 'text-amber-500' :
    loop.status === 'blocked' ? 'text-red-500' :
    'text-zinc-400';
  return (
    <div className="flex items-start gap-2 text-sm">
      <button onClick={cross} disabled={busy} title={done ? 'Reopen' : 'Mark done'} className="mt-0.5 shrink-0">
        {done ? <Check className="size-3.5 text-emerald-500" /> : <Circle className="size-3.5 text-zinc-400 hover:text-emerald-500" />}
      </button>
      <div className="flex-1">
        <span className={done ? 'line-through text-zinc-400' : ''}>{loop.text}</span>
        <span className={`ml-2 inline-flex items-center gap-1 text-[10px] uppercase tracking-wide ${statusColor}`}>
          <CircleDot className="size-2.5" />{loop.status}
        </span>
        {loop.related_repo_path && (
          <span className="ml-1 text-[11px] text-zinc-500" title={loop.related_repo_path}>
            ↳ waiting on {loop.related_repo_path.split('/').pop()}
          </span>
        )}
      </div>
    </div>
  );
}

function RepoView({ repos }: { repos: RepoGroup[] }) {
  // Surface overlaps (>1 session) first. useMemo must run before any
  // early return to satisfy the rules of hooks.
  const sorted = useMemo(
    () => [...(repos ?? [])].sort((a, b) => b.sessions.length - a.sessions.length),
    [repos],
  );
  if (!repos || repos.length === 0) {
    return <Empty>No repos tracked yet.</Empty>;
  }
  return (
    <div className="space-y-3">
      {sorted.map((g) => (
        <div key={g.path} className="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-4">
          <div className="flex items-center gap-2 mb-2">
            <FolderGit2 className="size-4 text-zinc-500" />
            <span className="font-mono text-sm font-medium">{g.dirname || g.path}</span>
            {g.branch && (
              <span className="inline-flex items-center gap-0.5 text-[11px] text-zinc-500">
                <GitBranch className="size-3" />{g.branch}
              </span>
            )}
            {(g.sessions?.length ?? 0) > 1 && (
              <span className="rounded-full bg-amber-500/15 text-amber-700 dark:text-amber-300 px-1.5 py-0.5 text-[10px] font-medium uppercase tracking-wider">
                {g.sessions.length} sessions
              </span>
            )}
          </div>
          <div className="space-y-1 pl-6">
            {(g.sessions ?? []).map((s) => (
              <div key={s.session_uuid} className="flex items-center gap-2 text-sm">
                <span className={[
                  'rounded px-1 text-[10px] uppercase tracking-wider',
                  s.role === 'primary' ? 'text-rose-600 dark:text-rose-400' : 'text-zinc-500',
                ].join(' ')}>
                  {s.role}
                </span>
                <Link to={`/sessions/${s.session_uuid}`} className="hover:text-rose-500 truncate">
                  {s.headline || s.session_uuid.slice(0, 8)}
                </Link>
              </div>
            ))}
          </div>
        </div>
      ))}
    </div>
  );
}

function Empty({ children }: { children: React.ReactNode }) {
  return (
    <div className="text-sm text-zinc-500 text-center py-12 rounded-lg border border-dashed border-zinc-300 dark:border-zinc-700">
      {children}
    </div>
  );
}
