import { fmtAbs, fmtDuration, pctBgColor, pctColor } from '../lib/format';

type Props = {
  label: string;
  pct: number;
  resetTS?: string;
  windowStartTS?: string;
  burnPctPerHour?: number;
  burnOK: boolean;
  etaMS?: number;
  etaTS?: string;
};

/**
 * UsageGauge shows one bucket's state: percentage, the actual reset window
 * (anchored to /usage's reset_ts when we have it), burn rate, and ETA.
 * Designed to be glanceable; no chart, no interaction.
 */
export default function UsageGauge({
  label,
  pct,
  resetTS,
  windowStartTS,
  burnPctPerHour,
  burnOK,
  etaMS,
  etaTS,
}: Props) {
  const clampedPct = Math.max(0, Math.min(100, pct));
  return (
    <div className="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
      <div className="flex items-baseline justify-between mb-3">
        <span className="text-xs uppercase tracking-wider text-zinc-500">{label}</span>
        <span className={`text-3xl font-semibold tabular-nums ${pctColor(clampedPct)}`}>
          {clampedPct}%
        </span>
      </div>

      <div className="h-2 w-full rounded-full bg-zinc-200 dark:bg-zinc-800 overflow-hidden">
        <div
          className={`h-full rounded-full transition-all ${pctBgColor(clampedPct)}`}
          style={{ width: `${clampedPct}%` }}
        />
      </div>

      <div className="mt-4 grid grid-cols-2 gap-3 text-xs">
        <Field label="Window">
          {windowStartTS && resetTS ? (
            <span className="tabular-nums">
              {fmtAbs(windowStartTS)} → {fmtAbs(resetTS)}
            </span>
          ) : (
            <span className="text-zinc-500">—</span>
          )}
        </Field>
        <Field label="Burn rate">
          {burnOK && burnPctPerHour !== undefined ? (
            <span className="tabular-nums">
              {burnPctPerHour >= 0 ? '+' : ''}
              {burnPctPerHour.toFixed(1)}%/h
            </span>
          ) : (
            <span className="text-zinc-500">need more data</span>
          )}
        </Field>
        <Field label="ETA to 100%">
          {etaMS !== undefined && etaTS ? (
            <span className="tabular-nums">{fmtDuration(etaMS)}</span>
          ) : (
            <span className="text-zinc-500">—</span>
          )}
        </Field>
        <Field label="ETA at">
          {etaTS ? (
            <span className="tabular-nums">{fmtAbs(etaTS)}</span>
          ) : (
            <span className="text-zinc-500">—</span>
          )}
        </Field>
      </div>
    </div>
  );
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div>
      <div className="text-[10px] uppercase tracking-wider text-zinc-500 mb-0.5">{label}</div>
      <div className="text-zinc-700 dark:text-zinc-300">{children}</div>
    </div>
  );
}
