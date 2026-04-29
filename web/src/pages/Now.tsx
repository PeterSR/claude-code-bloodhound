import { Activity, AlertCircle, RefreshCw } from 'lucide-react';
import UsageGauge from '../components/UsageGauge';
import { useApi } from '../hooks/useApi';
import { fmtAbs, fmtRel } from '../lib/format';

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
          className="flex items-center gap-1.5 text-xs text-zinc-500 hover:text-zinc-900 dark:hover:text-zinc-100 transition"
          title="Refresh"
        >
          <RefreshCw className="size-3.5" />
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
          <div className="grid gap-4 grid-cols-1 lg:grid-cols-2 max-w-4xl mt-4">
            {data.session ? (
              <UsageGauge
                label="Session (5h)"
                pct={data.session.pct}
                resetTS={data.session.reset_ts}
                windowStartTS={data.session.window_start_ts}
                timeToResetMS={data.session.time_to_reset_ms}
                burnPctPerHour={data.session.burn_pct_per_hour}
                burnOK={data.session.burn_ok}
                limitOK={data.session.limit_ok}
                limitETAMS={data.session.limit_eta_ms}
                limitETATS={data.session.limit_eta_ts}
              />
            ) : (
              <Empty label="Session (5h)" />
            )}
            {data.week ? (
              <UsageGauge
                label="Week"
                pct={data.week.pct}
                resetTS={data.week.reset_ts}
                windowStartTS={data.week.window_start_ts}
                timeToResetMS={data.week.time_to_reset_ms}
                burnPctPerHour={data.week.burn_pct_per_hour}
                burnOK={data.week.burn_ok}
                limitOK={data.week.limit_ok}
                limitETAMS={data.week.limit_eta_ms}
                limitETATS={data.week.limit_eta_ts}
              />
            ) : (
              <Empty label="Week" />
            )}
          </div>
        </>
      )}
    </div>
  );
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

function Empty({ label }: { label: string }) {
  return (
    <div className="rounded-lg border border-dashed border-zinc-300 dark:border-zinc-700 p-5">
      <div className="text-xs uppercase tracking-wider text-zinc-500">{label}</div>
      <div className="mt-2 text-zinc-500">No data yet.</div>
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
