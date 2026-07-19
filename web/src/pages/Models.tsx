import { useMemo, useState } from 'react';
import {
  Boxes,
  TriangleAlert,
  Package,
  FileCog,
  Search,
  Loader2,
  ExternalLink,
} from 'lucide-react';
import ReloadButton from '../components/ReloadButton';
import { useApi } from '../hooks/useApi';
import { fmtNumber } from '../lib/format';
import { modelColors, modelLabel } from '../lib/models';

type ModelTotal = {
  model: string;
  turns: number;
  raw_tokens: number;
  cw_tokens: number;
  share: number;
  multiplier: number;
  known: boolean;
  input_price?: number;
  output_price?: number;
  breaks_ratio?: boolean;
  unverified?: boolean;
  source?: string;
};

type PriceTableInfo = {
  origin: string; // "user" | "default"
  version: number;
  baseline: string;
  generated_at?: string;
  generated_by?: string;
  priced_count: number;
};

type ModelDay = {
  ts_unix_ms: number;
  cw_tokens: Record<string, number>;
  total_cw_tokens: number;
};

type ModelWeight = {
  model: string;
  samples: number;
  tokens_per_pct: number;
  weight: number;
  list_weight: number;
  is_baseline: boolean;
  diverges: boolean;
};

type WeightsReport = {
  rows: ModelWeight[];
  baseline: string;
  baseline_ok: boolean;
  since_unix_ms: number;
  days: number;
  bucket: string;
  min_samples: number;
  control?: { a: string; b: string; ratio: number };
};

type ModelsResponse = {
  ok: boolean;
  window_days: number;
  models: ModelTotal[];
  unpriced: ModelTotal[];
  days: ModelDay[];
  total_cw_tokens: number;
  prices: PriceTableInfo;
  weights: WeightsReport;
};

const WINDOW_OPTIONS = [
  { label: '7d', days: 7 },
  { label: '30d', days: 30 },
  { label: '90d', days: 90 },
];

export default function Models() {
  const [days, setDays] = useState(30);
  const [mode, setMode] = useState<'volume' | 'share'>('volume');
  const { data, error, loading, refreshing, refresh } = useApi<ModelsResponse>(
    `/models?window_days=${days}`,
    60_000,
  );

  const colors = useMemo(
    () => modelColors(data?.models.map((m) => m.model) ?? []),
    [data],
  );
  // Stacking order follows the totals table so the legend, the table and
  // the bars all read in the same order.
  const order = useMemo(() => data?.models.map((m) => m.model) ?? [], [data]);

  return (
    <div>
      <div className="flex items-center justify-between mb-2">
        <div className="flex items-center gap-2">
          <Boxes className="size-5 text-zinc-600 dark:text-zinc-400" />
          <h1 className="text-2xl font-semibold tracking-tight">Models</h1>
        </div>
        <div className="flex items-center gap-3">
          <Toggle
            options={WINDOW_OPTIONS.map((o) => ({ label: o.label, value: o.days }))}
            value={days}
            onChange={setDays}
          />
          <ReloadButton refreshing={refreshing} onClick={refresh} />
        </div>
      </div>
      <p className="text-zinc-600 dark:text-zinc-400 max-w-2xl mb-6">
        Which models your limit actually went to, and what each one really
        costs you. Every turn records the model that served it, so the split
        below is measured, not estimated.
      </p>

      {error && (
        <div className="text-sm text-red-600 dark:text-red-400 mb-4">{error.message}</div>
      )}
      {loading && !data && <div className="text-sm text-zinc-500">Loading…</div>}

      {data && data.models.length === 0 && (
        <div className="text-sm text-zinc-500">
          No turns ingested in this window.
        </div>
      )}

      {data && data.models.length > 0 && (
        <div className="space-y-8">
          <Section
            title="Where your limit went"
            subtitle={
              <>
                Share of{' '}
                <em className="not-italic font-medium text-zinc-600 dark:text-zinc-300">
                  cost-weighted
                </em>{' '}
                tokens, which is also share of limit burned: cost-weighted
                tokens are the model of what moves the percentage, so the two
                are the same number by construction. Weighting is by token
                type (input ×1, output ×5, cache_read ×0.1, cache_create_5m
                ×1.25, cache_create_1h ×2) times the model's price relative to
                Opus.
              </>
            }
          >
            <div className="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
              <div className="flex items-center justify-between gap-3 mb-4">
                <div className="text-sm font-medium text-zinc-700 dark:text-zinc-300">
                  {mode === 'volume' ? 'Daily burn by model' : 'Daily mix by model'}
                </div>
                <Toggle
                  options={[
                    { label: 'Volume', value: 'volume' as const },
                    { label: 'Share', value: 'share' as const },
                  ]}
                  value={mode}
                  onChange={setMode}
                />
              </div>
              <StackedDays days={data.days} order={order} colors={colors} mode={mode} />
              <Legend models={order} colors={colors} />
            </div>

            <div className="mt-5">
              <TotalsTable
                models={data.models}
                colors={colors}
                totalCW={data.total_cw_tokens}
              />
              <PriceProvenance prices={data.prices} />
            </div>

            {data.unpriced.length > 0 && (
              <div className="mt-5">
                <UnpricedSection models={data.unpriced} onHealed={refresh} />
              </div>
            )}
          </Section>

          <WeightsSection weights={data.weights} colors={colors} />
        </div>
      )}
    </div>
  );
}

/** Height in px of the daily-burn chart. */
const CHART_H = 176;

function StackedDays({
  days,
  order,
  colors,
  mode,
}: {
  days: ModelDay[];
  order: string[];
  colors: Record<string, string>;
  mode: 'volume' | 'share';
}) {
  const max = Math.max(1, ...days.map((d) => d.total_cw_tokens));
  if (days.length === 0) return <Empty />;

  return (
    <div className="flex items-end gap-px" style={{ height: CHART_H }}>
      {days.map((d) => {
        const total = d.total_cw_tokens;
        // In volume mode the bar's height encodes the day's total burn and
        // its segments encode the mix, so a heavy day dominated by an
        // expensive model is visible as one shape rather than two charts.
        const barPct = mode === 'share' ? (total > 0 ? 100 : 0) : (total / max) * 100;
        const date = new Date(d.ts_unix_ms).toLocaleDateString(undefined, {
          month: 'short',
          day: 'numeric',
        });
        return (
          <div
            key={d.ts_unix_ms}
            className="flex-1 flex flex-col justify-end min-w-0"
            style={{ height: '100%' }}
          >
            <div
              className="flex flex-col-reverse w-full rounded-sm overflow-hidden"
              style={{ height: `${barPct}%` }}
            >
              {order.map((m) => {
                const cw = d.cw_tokens[m] ?? 0;
                if (cw <= 0 || total <= 0) return null;
                return (
                  <div
                    key={m}
                    style={{ height: `${(cw / total) * 100}%`, background: colors[m] }}
                    title={`${date} — ${modelLabel(m)}: ${fmtNumber(cw)} cw (${(
                      (cw / total) * 100
                    ).toFixed(1)}% of the day)`}
                  />
                );
              })}
            </div>
          </div>
        );
      })}
    </div>
  );
}

function Legend({
  models,
  colors,
}: {
  models: string[];
  colors: Record<string, string>;
}) {
  return (
    <div className="flex flex-wrap gap-x-4 gap-y-1 mt-3">
      {models.map((m) => (
        <div key={m} className="flex items-center gap-1.5 text-xs text-zinc-500">
          <span
            className="size-2 rounded-sm shrink-0"
            style={{ background: colors[m] }}
          />
          {modelLabel(m)}
        </div>
      ))}
    </div>
  );
}

function TotalsTable({
  models,
  colors,
  totalCW,
}: {
  models: ModelTotal[];
  colors: Record<string, string>;
  totalCW: number;
}) {
  return (
    <div className="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 overflow-hidden">
      <table className="w-full text-sm">
        <thead>
          <tr className="text-xs uppercase tracking-wider text-zinc-500 border-b border-zinc-200 dark:border-zinc-800">
            <th className="text-left font-normal px-4 py-2.5">Model</th>
            <th className="text-right font-normal px-4 py-2.5">Share of burn</th>
            <th className="text-right font-normal px-4 py-2.5">Cost-weighted</th>
            <th className="text-right font-normal px-4 py-2.5">Raw tokens</th>
            <th className="text-right font-normal px-4 py-2.5">Turns</th>
            <th className="text-right font-normal px-4 py-2.5">Price</th>
          </tr>
        </thead>
        <tbody>
          {models.map((m) => (
            <tr
              key={m.model}
              className="border-b border-zinc-100 dark:border-zinc-800/60 last:border-0"
            >
              <td className="px-4 py-2.5">
                <div className="flex items-center gap-2">
                  <span
                    className="size-2.5 rounded-sm shrink-0"
                    style={{ background: colors[m.model] }}
                  />
                  <span className="font-medium">{modelLabel(m.model)}</span>
                  {m.unverified && (
                    <a
                      href={m.source || undefined}
                      target="_blank"
                      rel="noreferrer"
                      className="flex items-center gap-0.5 text-[10px] text-sky-600 dark:text-sky-400 hover:underline"
                      title={`Price auto-found${
                        m.source ? ` from ${m.source}` : ''
                      } and not yet corroborated by your local calibration. Compare it against the measured weight below.`}
                    >
                      unverified
                      {m.source && <ExternalLink className="size-2.5" />}
                    </a>
                  )}
                  {m.breaks_ratio && (
                    <span
                      className="flex items-center gap-0.5 text-[10px] text-amber-600 dark:text-amber-500"
                      title="This model prices output at something other than 5x its input, which is the assumption the single-scalar weighting relies on. Its multiplier is an approximation."
                    >
                      <TriangleAlert className="size-3" />
                      approx
                    </span>
                  )}
                </div>
              </td>
              <td className="px-4 py-2.5">
                <div className="flex items-center justify-end gap-2">
                  <div className="w-24 h-1.5 rounded-full bg-zinc-100 dark:bg-zinc-800 overflow-hidden">
                    <div
                      className="h-full rounded-full"
                      style={{
                        width: `${m.share * 100}%`,
                        background: colors[m.model],
                      }}
                    />
                  </div>
                  <span className="tabular-nums w-12 text-right">
                    {(m.share * 100).toFixed(1)}%
                  </span>
                </div>
              </td>
              <td className="px-4 py-2.5 text-right tabular-nums">
                {fmtNumber(m.cw_tokens)}
              </td>
              <td className="px-4 py-2.5 text-right tabular-nums text-zinc-500">
                {fmtNumber(m.raw_tokens)}
              </td>
              <td className="px-4 py-2.5 text-right tabular-nums text-zinc-500">
                {fmtNumber(m.turns)}
              </td>
              <td
                className="px-4 py-2.5 text-right tabular-nums text-zinc-500"
                title={
                  m.input_price
                    ? `$${m.input_price}/$${m.output_price} per million tokens (in/out) — ${m.multiplier}× the baseline's input price`
                    : 'List price per token relative to the baseline'
                }
              >
                {m.input_price ? (
                  <span className="whitespace-nowrap">
                    ${m.input_price}
                    <span className="text-zinc-400 dark:text-zinc-600">
                      /${m.output_price}
                    </span>
                  </span>
                ) : (
                  `${m.multiplier}×`
                )}
              </td>
            </tr>
          ))}
        </tbody>
        <tfoot>
          <tr className="border-t border-zinc-200 dark:border-zinc-800 text-zinc-500">
            <td className="px-4 py-2.5 text-xs uppercase tracking-wider">Total</td>
            <td />
            <td className="px-4 py-2.5 text-right tabular-nums">
              {fmtNumber(totalCW)}
            </td>
            <td colSpan={3} />
          </tr>
        </tfoot>
      </table>
    </div>
  );
}

function PriceProvenance({ prices }: { prices: PriceTableInfo }) {
  const isUser = prices.origin === 'user';
  return (
    <div className="mt-2 flex items-center gap-1.5 text-xs text-zinc-500">
      {isUser ? (
        <FileCog className="size-3.5 shrink-0 text-sky-500" />
      ) : (
        <Package className="size-3.5 shrink-0" />
      )}
      <span>
        {isUser ? (
          <>
            Prices from your local table
            {prices.generated_by ? ` (${prices.generated_by})` : ''}
          </>
        ) : (
          <>Prices from the bundled defaults</>
        )}{' '}
        · baseline{' '}
        <span className="text-zinc-600 dark:text-zinc-400">
          {modelLabel(prices.baseline)}
        </span>{' '}
        · {prices.priced_count} models priced. The multiplier is each model's
        input price over the baseline's, a documented proxy for how the limiter
        weights it — not a measurement. The section below measures it.
      </span>
    </div>
  );
}

type HealState = { status: 'idle' | 'running' | 'done'; message?: string };

function UnpricedSection({
  models,
  onHealed,
}: {
  models: ModelTotal[];
  onHealed: () => void;
}) {
  const [heal, setHeal] = useState<Record<string, HealState>>({});

  const runHeal = async (model: string) => {
    setHeal((h) => ({ ...h, [model]: { status: 'running' } }));
    try {
      const res = await fetch('/api/prices/heal', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ model }),
      });
      const body = await res.json();
      if (body.saved) {
        // The model is priced now; refreshing drops it from this section.
        setHeal((h) => ({ ...h, [model]: { status: 'done' } }));
        onHealed();
      } else {
        setHeal((h) => ({
          ...h,
          [model]: {
            status: 'idle',
            message: body.reason || 'No price found. Try again or add it by hand.',
          },
        }));
      }
    } catch (e) {
      setHeal((h) => ({
        ...h,
        [model]: {
          status: 'idle',
          message: e instanceof Error ? e.message : String(e),
        },
      }));
    }
  };

  return (
    <div className="rounded-lg border border-amber-300/60 dark:border-amber-700/40 bg-amber-50/50 dark:bg-amber-950/20 p-5">
      <div className="flex items-center gap-2 mb-1">
        <TriangleAlert className="size-4 text-amber-500 shrink-0" />
        <div className="text-sm font-medium text-zinc-700 dark:text-zinc-300">
          Unpriced models
        </div>
      </div>
      <div className="text-xs text-zinc-500 mb-4 max-w-3xl">
        Seen in this window but absent from the price table, so there's no
        honest weight for them. They're held out of the cost-weighted share
        above rather than folded in at a guessed rate — you're seeing their raw
        volume instead. The daemon looks these up automatically when it can; you
        can also trigger a lookup now, or add a price by hand (edit{' '}
        <code className="font-mono">prices.json</code> in the state directory).
      </div>
      <table className="w-full text-sm">
        <thead>
          <tr className="text-xs uppercase tracking-wider text-zinc-500 border-b border-amber-200/60 dark:border-amber-800/40">
            <th className="text-left font-normal py-2">Model</th>
            <th className="text-right font-normal py-2">Raw tokens</th>
            <th className="text-right font-normal py-2">Turns</th>
            <th className="py-2"></th>
          </tr>
        </thead>
        <tbody>
          {models.map((m) => {
            const state = heal[m.model] ?? { status: 'idle' };
            return (
              <tr
                key={m.model}
                className="border-b border-amber-100/50 dark:border-amber-900/30 last:border-0"
              >
                <td className="py-2 font-medium align-top">
                  {modelLabel(m.model)}
                  {state.message && (
                    <div className="text-[11px] font-normal text-amber-700 dark:text-amber-500 mt-0.5">
                      {state.message}
                    </div>
                  )}
                </td>
                <td className="py-2 text-right tabular-nums text-zinc-500 align-top">
                  {fmtNumber(m.raw_tokens)}
                </td>
                <td className="py-2 text-right tabular-nums text-zinc-500 align-top">
                  {fmtNumber(m.turns)}
                </td>
                <td className="py-2 text-right align-top">
                  <button
                    onClick={() => runHeal(m.model)}
                    disabled={state.status === 'running'}
                    className="inline-flex items-center gap-1.5 px-2.5 py-1 rounded-md border border-amber-300 dark:border-amber-700/60 text-xs hover:bg-amber-100/70 dark:hover:bg-amber-900/30 transition disabled:opacity-50 disabled:cursor-not-allowed"
                    title="Ask Claude to look up this model's list price on the web. Saved as unverified until local calibration corroborates it."
                  >
                    {state.status === 'running' ? (
                      <Loader2 className="size-3.5 animate-spin" />
                    ) : (
                      <Search className="size-3.5" />
                    )}
                    {state.status === 'running' ? 'Looking…' : 'Find out for me'}
                  </button>
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

function WeightsSection({
  weights,
  colors,
}: {
  weights: WeightsReport;
  colors: Record<string, string>;
}) {
  const measured = weights.rows.filter((r) => r.weight > 0 && !r.is_baseline);
  return (
    <Section
      title="What each model really costs"
      subtitle={
        <>
          The table above prices models by Anthropic's published API ratios.
          Those are a documented, stable proxy for how the subscription rate
          limiter weights a model — but they are not a measurement of it, and
          Anthropic doesn't publish one. This is Bloodhound measuring it from
          your own data: for limit windows dominated by a single model, how
          many tokens it took to burn 1%. Measured over the last{' '}
          {weights.days} days, against {modelLabel(weights.baseline)}.
        </>
      }
    >
      {!weights.baseline_ok ? (
        <Note>
          Not enough data to measure anything yet. Every weight is relative to{' '}
          {modelLabel(weights.baseline)}, and there aren't{' '}
          {weights.min_samples} limit windows dominated by it in the last{' '}
          {weights.days} days to anchor the scale.
        </Note>
      ) : (
        <div className="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 overflow-hidden">
          <table className="w-full text-sm">
            <thead>
              <tr className="text-xs uppercase tracking-wider text-zinc-500 border-b border-zinc-200 dark:border-zinc-800">
                <th className="text-left font-normal px-4 py-2.5">Model</th>
                <th className="text-right font-normal px-4 py-2.5">List price</th>
                <th className="text-right font-normal px-4 py-2.5">Measured here</th>
                <th className="text-right font-normal px-4 py-2.5">Windows</th>
                <th className="text-left font-normal px-4 py-2.5"></th>
              </tr>
            </thead>
            <tbody>
              {weights.rows.map((r) => (
                <WeightRow key={r.model} row={r} color={colors[r.model]} minSamples={weights.min_samples} />
              ))}
            </tbody>
          </table>
        </div>
      )}

      <div className="mt-3 space-y-1.5 text-xs text-zinc-500 max-w-3xl">
        {measured.some((r) => r.diverges) && (
          <p className="flex gap-1.5">
            <TriangleAlert className="size-3.5 shrink-0 mt-px text-amber-500" />
            <span>
              Flagged rows burn your limit at a rate list price doesn't
              predict. Where they disagree, the measured number is the one
              describing your actual bill.
            </span>
          </p>
        )}
        <p>
          <strong className="font-medium text-zinc-600 dark:text-zinc-400">
            Why rows say “can't measure”:
          </strong>{' '}
          a model can only be measured over windows it dominates. One used
          alongside another the whole time — subagent work, typically — never
          dominates a window, so no amount of waiting will fill it in. That's
          a limit of the method, not missing data.
        </p>
        <p>
          <strong className="font-medium text-zinc-600 dark:text-zinc-400">
            Why only {weights.days} days:
          </strong>{' '}
          model adoption tracks time — you use one for a month, then the next.
          Measured across eras, this comparison silently absorbs changes in
          the limit regime as if they were properties of the model. Widening
          the window makes it worse, not better.
        </p>
        {weights.control ? (
          <p>
            <strong className="font-medium text-zinc-600 dark:text-zinc-400">
              Error bar:
            </strong>{' '}
            {modelLabel(weights.control.a)} and {modelLabel(weights.control.b)}{' '}
            list at the same price, so a perfect estimator would put them
            1.00× apart. It puts them{' '}
            <span className="tabular-nums">{weights.control.ratio.toFixed(2)}×</span>{' '}
            apart, which is this method's noise floor. Don't believe any
            divergence smaller than that.
          </p>
        ) : (
          <p>
            <strong className="font-medium text-zinc-600 dark:text-zinc-400">
              No error bar:
            </strong>{' '}
            calibrating the method needs two identically-priced models used in
            the same period, which hasn't happened in this window. So the
            measured weights have no independent check on them.
          </p>
        )}
      </div>
    </Section>
  );
}

function WeightRow({
  row,
  color,
  minSamples,
}: {
  row: ModelWeight;
  color: string;
  minSamples: number;
}) {
  const measurable = row.weight > 0;
  return (
    <tr className="border-b border-zinc-100 dark:border-zinc-800/60 last:border-0">
      <td className="px-4 py-2.5">
        <div className="flex items-center gap-2">
          <span className="size-2.5 rounded-sm shrink-0" style={{ background: color }} />
          <span className="font-medium">{modelLabel(row.model)}</span>
          {row.is_baseline && (
            <span className="text-[10px] uppercase tracking-wider text-zinc-400 dark:text-zinc-600">
              baseline
            </span>
          )}
        </div>
      </td>
      <td className="px-4 py-2.5 text-right tabular-nums text-zinc-500">
        {row.list_weight}×
      </td>
      <td className="px-4 py-2.5 text-right tabular-nums">
        {measurable ? (
          <span
            className={
              row.diverges ? 'text-amber-600 dark:text-amber-500 font-medium' : ''
            }
          >
            {row.weight.toFixed(2)}×
          </span>
        ) : (
          <span className="text-zinc-400 dark:text-zinc-600">—</span>
        )}
      </td>
      <td className="px-4 py-2.5 text-right tabular-nums text-zinc-500">
        {row.samples || '—'}
      </td>
      <td className="px-4 py-2.5 text-xs">
        {measurable && row.diverges && (
          <span className="flex items-center gap-1 text-amber-600 dark:text-amber-500">
            <TriangleAlert className="size-3.5 shrink-0" />
            {row.weight > row.list_weight
              ? `${(row.weight / row.list_weight).toFixed(1)}× costlier than list implies`
              : `cheaper than list implies`}
          </span>
        )}
        {!measurable && (
          <span className="text-zinc-400 dark:text-zinc-600">
            {row.samples === 0
              ? "can't measure — never used on its own"
              : `can't measure — ${row.samples}/${minSamples} windows`}
          </span>
        )}
      </td>
    </tr>
  );
}

function Toggle<T extends string | number>({
  options,
  value,
  onChange,
}: {
  options: { label: string; value: T }[];
  value: T;
  onChange: (v: T) => void;
}) {
  return (
    <div className="flex rounded-md border border-zinc-300 dark:border-zinc-700 overflow-hidden text-xs">
      {options.map((o) => (
        <button
          key={String(o.value)}
          onClick={() => onChange(o.value)}
          className={[
            'px-3 py-1',
            value === o.value
              ? 'bg-zinc-900 dark:bg-zinc-100 text-white dark:text-zinc-900'
              : 'bg-white dark:bg-zinc-900 hover:bg-zinc-100 dark:hover:bg-zinc-800',
          ].join(' ')}
        >
          {o.label}
        </button>
      ))}
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

function Note({ children }: { children: React.ReactNode }) {
  return (
    <div className="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5 text-sm text-zinc-500">
      {children}
    </div>
  );
}

function Empty() {
  return (
    <div className="text-sm text-zinc-500 text-center py-12">Not enough data yet.</div>
  );
}
