import { useState } from 'react';
import { Bug, RefreshCw, Wand2, Loader2, Check, X } from 'lucide-react';
import { useApi } from '../hooks/useApi';
import { fmtAbs, fmtRel, fmtNumber } from '../lib/format';

type DebugResponse = {
  build: { version: string; commit: string; date: string };
  paths: Record<string, string>;
  device_id: string;
  schema_version: number;
  extractor: {
    origin: string;
    version: number;
    generated_at?: string;
    generated_by?: string;
    field_count: number;
  };
  ingest: {
    files_tracked: number;
    turns_total: number;
    compactions_confirmed: number;
    latest_turn_ts?: string;
  };
  last_poll?: {
    ts: string;
    age_s: number;
    parse_ok: boolean;
    elapsed_s: number;
    session_pct?: number;
    week_pct?: number;
    session_reset_raw?: string;
    week_reset_raw?: string;
  };
  raw_dump?: { ts: string; tail: string; bytes_total: number };
  aggregate: {
    bucket_count: number;
    session_count: number;
    latest_bucket?: {
      start_ts: string;
      end_ts: string;
      reset_inferred: boolean;
      raw_tokens: number;
      cost_weighted_tokens: number;
      output_tokens: number;
      turn_count: number;
    };
  };
  server_now: string;
};

export default function Debug() {
  const { data, error, loading, refresh } = useApi<DebugResponse>('/debug', 60_000);

  return (
    <div>
      <div className="flex items-center justify-between mb-2">
        <div className="flex items-center gap-2">
          <Bug className="size-5 text-amber-500" />
          <h1 className="text-2xl font-semibold tracking-tight">Debug</h1>
        </div>
        <button
          onClick={refresh}
          className="flex items-center gap-1.5 text-xs text-zinc-500 hover:text-zinc-900 dark:hover:text-zinc-100 transition"
        >
          <RefreshCw className="size-3.5" />
          Refresh
        </button>
      </div>
      <p className="text-zinc-600 dark:text-zinc-400 max-w-2xl mb-6">
        Internal state. Useful when something looks wrong — the raw <code className="font-mono text-xs">/usage</code> dump,
        the parser output, ingest counters, and what the active extractor came from.
      </p>

      {error && (
        <div className="text-sm text-red-600 dark:text-red-400 mb-4">
          {error.message}
        </div>
      )}

      {loading && !data && <div className="text-sm text-zinc-500">Loading…</div>}

      {data && (
        <div className="space-y-4 max-w-5xl">
          <Section title="Build">
            <KV k="version" v={data.build.version} />
            <KV k="commit" v={data.build.commit} />
            <KV k="schema" v={String(data.schema_version)} />
            <KV k="device_id" v={data.device_id} mono />
            <KV k="server now" v={data.server_now} mono />
          </Section>

          <Section title="Extractor">
            <KV k="origin" v={data.extractor.origin} />
            <KV k="version" v={String(data.extractor.version)} />
            <KV k="generated" v={data.extractor.generated_at ? `${data.extractor.generated_at} by ${data.extractor.generated_by}` : '—'} />
            <KV k="field count" v={String(data.extractor.field_count)} />
          </Section>

          <RetrainCard onDone={refresh} />

          <Section title="Last poll">
            {data.last_poll ? (
              <>
                <KV k="ts" v={`${fmtAbs(data.last_poll.ts)} (${fmtRel(data.last_poll.age_s)})`} />
                <KV k="parse_ok" v={String(data.last_poll.parse_ok)} status={data.last_poll.parse_ok ? 'ok' : 'fail'} />
                <KV k="elapsed" v={`${data.last_poll.elapsed_s.toFixed(1)}s`} />
                <KV k="session_pct" v={data.last_poll.session_pct ?? '—'} />
                <KV k="week_pct" v={data.last_poll.week_pct ?? '—'} />
                <KV k="session_reset_raw" v={data.last_poll.session_reset_raw ?? '—'} mono />
                <KV k="week_reset_raw" v={data.last_poll.week_reset_raw ?? '—'} mono />
              </>
            ) : (
              <div className="text-zinc-500 text-sm">No polls yet.</div>
            )}
          </Section>

          <Section title="Ingest">
            <KV k="files tracked" v={fmtNumber(data.ingest.files_tracked)} />
            <KV k="turns total" v={fmtNumber(data.ingest.turns_total)} />
            <KV k="compactions confirmed" v={fmtNumber(data.ingest.compactions_confirmed)} />
            <KV k="latest turn" v={data.ingest.latest_turn_ts ? fmtAbs(data.ingest.latest_turn_ts) : '—'} />
          </Section>

          <Section title="Aggregate">
            <KV k="sessions" v={fmtNumber(data.aggregate.session_count)} />
            <KV k="buckets" v={fmtNumber(data.aggregate.bucket_count)} />
            {data.aggregate.latest_bucket && (
              <>
                <KV k="latest bucket window" v={`${fmtAbs(data.aggregate.latest_bucket.start_ts)} → ${fmtAbs(data.aggregate.latest_bucket.end_ts)}`} />
                <KV k="latest bucket raw" v={fmtNumber(data.aggregate.latest_bucket.raw_tokens)} />
                <KV k="latest bucket cost-weighted" v={fmtNumber(data.aggregate.latest_bucket.cost_weighted_tokens)} />
                <KV k="latest bucket reset_inferred" v={String(data.aggregate.latest_bucket.reset_inferred)} />
              </>
            )}
          </Section>

          <Section title="Paths">
            {Object.entries(data.paths).map(([k, v]) => (
              <KV key={k} k={k} v={v} mono />
            ))}
          </Section>

          {data.raw_dump && (
            <Section title="Last /usage raw dump">
              <div className="text-xs text-zinc-500 mb-2">
                {fmtAbs(data.raw_dump.ts)} · {fmtNumber(data.raw_dump.bytes_total)} bytes
              </div>
              <pre className="text-[11px] leading-snug bg-zinc-100 dark:bg-zinc-950 border border-zinc-200 dark:border-zinc-800 rounded-md p-3 overflow-auto max-h-96 whitespace-pre-wrap break-words">
                {data.raw_dump.tail}
              </pre>
            </Section>
          )}
        </div>
      )}
    </div>
  );
}

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-4">
      <div className="text-xs uppercase tracking-wider text-zinc-500 mb-3">{title}</div>
      <div className="grid grid-cols-1 md:grid-cols-2 gap-x-6 gap-y-1.5 text-sm">
        {children}
      </div>
    </div>
  );
}

type RetrainResult = {
  ok: boolean;
  elapsed_s?: number;
  fields?: number;
  saved_at?: string;
  applied?: boolean;
  missing?: string[];
  error?: string;
};

function RetrainCard({ onDone }: { onDone: () => void }) {
  const [running, setRunning] = useState(false);
  const [result, setResult] = useState<RetrainResult | null>(null);

  const run = async () => {
    setRunning(true);
    setResult(null);
    try {
      const res = await fetch('/api/extractor/retrain', { method: 'POST' });
      const body = (await res.json()) as RetrainResult;
      setResult(body);
      if (body.ok) onDone();
    } catch (e) {
      setResult({ ok: false, error: e instanceof Error ? e.message : String(e) });
    } finally {
      setRunning(false);
    }
  };

  return (
    <div className="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-4">
      <div className="text-xs uppercase tracking-wider text-zinc-500 mb-3">Retrain extractor</div>
      <div className="text-sm text-zinc-600 dark:text-zinc-400 mb-3 max-w-2xl">
        Captures a fresh <code className="font-mono text-xs">/usage</code> panel and asks the local <code className="font-mono text-xs">claude -p</code> to
        generate new extractor rules. Use when the daemon keeps reporting "extraction failed" and
        you don't want to wait for the next automatic self-heal (or you've turned auto self-heal off).
        Consumes one <code className="font-mono text-xs">claude -p</code> turn against your account; budget ~30–60s.
      </div>
      <div className="flex items-center gap-3">
        <button
          onClick={run}
          disabled={running}
          className="inline-flex items-center gap-2 px-3 py-1.5 rounded-md border border-zinc-300 dark:border-zinc-700 text-sm hover:bg-zinc-100 dark:hover:bg-zinc-900 transition disabled:opacity-50 disabled:cursor-not-allowed"
        >
          {running ? <Loader2 className="size-4 animate-spin" /> : <Wand2 className="size-4" />}
          {running ? 'Retraining…' : 'Retrain now'}
        </button>
        {result && !running && (
          <RetrainStatus result={result} />
        )}
      </div>
    </div>
  );
}

function RetrainStatus({ result }: { result: RetrainResult }) {
  if (!result.ok) {
    return (
      <div className="flex items-center gap-2 text-sm text-red-600 dark:text-red-400">
        <X className="size-4" />
        <span>Failed: {result.error ?? 'unknown error'}</span>
      </div>
    );
  }
  if (result.applied) {
    return (
      <div className="flex items-center gap-2 text-sm text-emerald-600 dark:text-emerald-400">
        <Check className="size-4" />
        <span>
          Done in {result.elapsed_s?.toFixed(1)}s · {result.fields} fields · all required fields extracted
        </span>
      </div>
    );
  }
  return (
    <div className="flex items-center gap-2 text-sm text-amber-600 dark:text-amber-400">
      <Check className="size-4" />
      <span>
        Saved in {result.elapsed_s?.toFixed(1)}s · {result.fields} fields · still missing: {result.missing?.join(', ') ?? '—'}
      </span>
    </div>
  );
}

function KV({ k, v, mono, status }: { k: string; v: string | number; mono?: boolean; status?: 'ok' | 'fail' }) {
  return (
    <div className="flex items-baseline gap-2">
      <span className="text-zinc-500 text-xs min-w-32">{k}</span>
      <span
        className={[
          'truncate',
          mono ? 'font-mono text-xs' : '',
          status === 'fail' ? 'text-red-600 dark:text-red-400' : '',
          status === 'ok' ? 'text-emerald-600 dark:text-emerald-400' : '',
        ].join(' ')}
        title={String(v)}
      >
        {v}
      </span>
    </div>
  );
}
