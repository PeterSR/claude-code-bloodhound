import { useState } from 'react';
import { Wallet, AlertTriangle, Info } from 'lucide-react';
import { useApi } from '../hooks/useApi';
import { apiPost } from '../api/client';
import ReloadButton from '../components/ReloadButton';

type Lease = {
  id: number;
  kind: string;
  at_ms?: number;
  bucket?: string;
  expired_ms?: number;
};

type Budget = {
  id: number;
  cwd: string;
  bucket: string;
  spend_pct?: number;
  meter_pct?: number;
  note?: string;
  set_ms: number;
  retired_ms?: number;
  retired_why?: string;
  leases?: Lease[];
  /** Recorded pressure. Absent until the reconciler has evaluated it. */
  state?: string;
  since_ms?: number;
  in_force: string;
};

type BudgetsResponse = {
  ok: boolean;
  server_now_ms: number;
  budgets: Budget[];
};

const BUCKET_LABEL: Record<string, string> = { session: '5h', week: 'week' };

/** Pressure states, in the order the sensor produces them. */
const STATE_STYLE: Record<string, { label: string; cls: string }> = {
  clear: { label: 'clear', cls: 'bg-emerald-500/10 text-emerald-700 dark:text-emerald-400' },
  tight: { label: 'tight', cls: 'bg-amber-500/10 text-amber-700 dark:text-amber-400' },
  exceeded: { label: 'exceeded', cls: 'bg-rose-500/10 text-rose-700 dark:text-rose-400' },
  unknown: { label: 'unknown', cls: 'bg-zinc-500/10 text-zinc-600 dark:text-zinc-400' },
};

export default function Budgets() {
  const { data, error, loading, refreshing, refresh } = useApi<BudgetsResponse>('/budgets', 30_000);
  const [showRetired, setShowRetired] = useState(false);

  const budgets = (data?.budgets || []).filter((b) => showRetired || !b.retired_ms);

  return (
    <div>
      <div className="flex items-center gap-2 mb-2">
        <Wallet className="size-5 text-sky-500" />
        <h1 className="text-2xl font-semibold tracking-tight">Budgets</h1>
        <div className="ml-auto">
          <ReloadButton refreshing={refreshing} onClick={refresh} />
        </div>
      </div>
      <p className="text-zinc-600 dark:text-zinc-400 max-w-2xl mb-6">
        What a working directory is allowed to spend. Bloodhound measures against
        it and mentions it to sessions that are already working; it never
        enforces anything. The 5h and weekly cliffs are watched whether or not
        you set a budget here.
      </p>

      {error && (
        <div className="text-sm text-red-600 dark:text-red-400 mb-4">{error.message}</div>
      )}
      {loading && !data && <div className="text-sm text-zinc-500">Loading…</div>}

      <NewBudget onSaved={refresh} />

      {data && (
        <>
          <div className="flex items-center gap-3 mt-8 mb-2">
            <h2 className="text-lg font-medium">In force</h2>
            <label className="ml-auto flex items-center gap-1.5 text-xs text-zinc-500">
              <input
                type="checkbox"
                checked={showRetired}
                onChange={(e) => setShowRetired(e.target.checked)}
              />
              show retired
            </label>
          </div>

          {budgets.length === 0 ? (
            <p className="text-sm text-zinc-500">
              No budgets set. Most directories do not need one: the limit guards
              above apply regardless.
            </p>
          ) : (
            <div className="overflow-x-auto">
              <table className="w-full text-sm">
                <thead>
                  <tr className="text-left text-xs uppercase tracking-wide text-zinc-500 border-b border-zinc-200 dark:border-zinc-800">
                    <th className="py-2 pr-4 font-medium">Directory</th>
                    <th className="py-2 pr-4 font-medium">Bucket</th>
                    <th className="py-2 pr-4 font-medium">Spend</th>
                    <th className="py-2 pr-4 font-medium">Meter</th>
                    <th className="py-2 pr-4 font-medium">Pressure</th>
                    <th className="py-2 pr-4 font-medium">In force</th>
                    <th className="py-2 font-medium"></th>
                  </tr>
                </thead>
                <tbody>
                  {budgets.map((b) => (
                    <BudgetRow key={b.id} b={b} onChanged={refresh} />
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </>
      )}
    </div>
  );
}

function BudgetRow({ b, onChanged }: { b: Budget; onChanged: () => void }) {
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const retired = !!b.retired_ms;

  // A budget the reconciler has not reached yet reads as pending, never as
  // clear: "we have not looked" and "there is room" are different answers and
  // showing the second for the first is how a dashboard lies.
  const style = b.state ? STATE_STYLE[b.state] || STATE_STYLE.unknown : null;

  const revoke = async () => {
    setBusy(true);
    setErr(null);
    try {
      await apiPost('/budgets?revoke=1', { cwd: b.cwd, bucket: b.bucket });
      onChanged();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <tr className={`border-b border-zinc-100 dark:border-zinc-900 ${retired ? 'opacity-50' : ''}`}>
      <td className="py-2 pr-4 font-mono text-xs break-all">{b.cwd}</td>
      <td className="py-2 pr-4">{BUCKET_LABEL[b.bucket] || b.bucket}</td>
      <td className="py-2 pr-4 tabular-nums">{b.spend_pct ? `${b.spend_pct}%` : '—'}</td>
      <td className="py-2 pr-4 tabular-nums">{b.meter_pct ? `${b.meter_pct}%` : '—'}</td>
      <td className="py-2 pr-4">
        {style ? (
          <span className={`px-1.5 py-0.5 rounded text-xs font-medium ${style.cls}`}>
            {style.label}
          </span>
        ) : (
          <span className="text-xs text-zinc-400">pending</span>
        )}
      </td>
      <td className="py-2 pr-4 text-xs text-zinc-500">{b.in_force}</td>
      <td className="py-2 text-right">
        {!retired && (
          <button
            onClick={revoke}
            disabled={busy}
            className="text-xs text-zinc-500 hover:text-rose-600 disabled:opacity-40"
          >
            revoke
          </button>
        )}
        {err && <div className="text-xs text-red-600">{err}</div>}
      </td>
    </tr>
  );
}

function NewBudget({ onSaved }: { onSaved: () => void }) {
  const [cwd, setCwd] = useState('');
  const [bucket, setBucket] = useState('week');
  const [spend, setSpend] = useState('');
  const [meter, setMeter] = useState('');
  const [until, setUntil] = useState('');
  const [note, setNote] = useState('');
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setErr(null);
    try {
      await apiPost('/budgets', {
        cwd,
        bucket,
        // Omit rather than send zero: a rule set to 0 and a rule not set are
        // different things, and the API reads 0 as "not set" only because
        // nothing sends it.
        spend_pct: spend ? Number(spend) : undefined,
        meter_pct: meter ? Number(meter) : undefined,
        until: until || undefined,
        note: note || undefined,
      });
      setSpend('');
      setMeter('');
      setNote('');
      onSaved();
    } catch (e) {
      setErr(e instanceof Error ? e.message : String(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form
      onSubmit={submit}
      className="rounded-lg border border-zinc-200 dark:border-zinc-800 p-4 max-w-3xl"
    >
      <h2 className="text-sm font-medium mb-3">Set a budget</h2>

      <div className="grid sm:grid-cols-2 gap-3">
        <label className="text-xs text-zinc-500 sm:col-span-2">
          Working directory
          <input
            required
            value={cwd}
            onChange={(e) => setCwd(e.target.value)}
            placeholder="/home/you/projects/myapp"
            className="mt-1 w-full rounded border border-zinc-300 dark:border-zinc-700 bg-transparent px-2 py-1 font-mono text-xs"
          />
        </label>

        <label className="text-xs text-zinc-500">
          Bucket
          <select
            value={bucket}
            onChange={(e) => setBucket(e.target.value)}
            className="mt-1 w-full rounded border border-zinc-300 dark:border-zinc-700 bg-transparent px-2 py-1 text-sm"
          >
            <option value="week">week</option>
            <option value="session">5h</option>
          </select>
        </label>

        <label className="text-xs text-zinc-500">
          In force until
          <input
            value={until}
            onChange={(e) => setUntil(e.target.value)}
            placeholder="reset, 2h, 18:00, 2026-08-20 (blank: until revoked)"
            className="mt-1 w-full rounded border border-zinc-300 dark:border-zinc-700 bg-transparent px-2 py-1 text-sm"
          />
        </label>

        <label className="text-xs text-zinc-500">
          Spend %
          <input
            type="number"
            min={0}
            max={100}
            value={spend}
            onChange={(e) => setSpend(e.target.value)}
            className="mt-1 w-full rounded border border-zinc-300 dark:border-zinc-700 bg-transparent px-2 py-1 text-sm tabular-nums"
          />
          <span className="block mt-1 text-[11px] text-zinc-400">
            movement this directory itself causes
          </span>
        </label>

        <label className="text-xs text-zinc-500">
          Meter %
          <input
            type="number"
            min={0}
            max={100}
            value={meter}
            onChange={(e) => setMeter(e.target.value)}
            className="mt-1 w-full rounded border border-zinc-300 dark:border-zinc-700 bg-transparent px-2 py-1 text-sm tabular-nums"
          />
          <span className="block mt-1 text-[11px] text-zinc-400">
            the shared reading to stop at, whoever moved it
          </span>
        </label>

        <label className="text-xs text-zinc-500 sm:col-span-2">
          Note
          <input
            value={note}
            onChange={(e) => setNote(e.target.value)}
            className="mt-1 w-full rounded border border-zinc-300 dark:border-zinc-700 bg-transparent px-2 py-1 text-sm"
          />
        </label>
      </div>

      <div className="flex items-start gap-2 mt-3 text-[11px] text-zinc-500">
        <Info className="size-3.5 shrink-0 mt-0.5" />
        <p>
          Set either rule or both. Spend needs attribution and so ignores what
          other projects do; meter watches the shared number and buys headroom
          at the top for ungoverned work.
        </p>
      </div>

      {err && (
        <div className="flex items-center gap-1.5 mt-3 text-xs text-red-600 dark:text-red-400">
          <AlertTriangle className="size-3.5" />
          {err}
        </div>
      )}

      <button
        type="submit"
        disabled={busy}
        className="mt-3 rounded bg-sky-600 hover:bg-sky-500 disabled:opacity-40 text-white text-sm px-3 py-1.5"
      >
        {busy ? 'Saving…' : 'Set budget'}
      </button>
    </form>
  );
}
