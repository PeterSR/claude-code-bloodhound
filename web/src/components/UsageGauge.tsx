import { AlertTriangle, Zap } from 'lucide-react';
import { fmtAbs, fmtDuration, pctBgColor, pctColor } from '../lib/format';

type Props = {
  label: string;
  pct: number;
  resetTS?: string;
  windowStartTS?: string;
  timeToResetMS?: number;
  burnPctPerHour?: number;
  burnOK: boolean;
  limitOK: boolean;
  limitETAMS?: number;
  limitETATS?: string;
  saturated?: boolean;
};

/**
 * UsageGauge shows one bucket's state. Two distinct time concepts:
 *
 *   Time to reset — always shown when known. The natural cycle boundary
 *   parsed from /usage; we are inside [window_start, reset].
 *
 *   Limit projection — only shown when the burn rate would actually hit
 *   100% before reset. Projecting "100% in 8 days" when the bucket
 *   resets in 3h is noise, not signal.
 */
export default function UsageGauge({
  label,
  pct,
  resetTS,
  windowStartTS,
  timeToResetMS,
  burnPctPerHour,
  burnOK,
  limitOK,
  limitETAMS,
  limitETATS,
  saturated,
}: Props) {
  const clampedPct = Math.max(0, Math.min(100, pct));
  return (
    <div className="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
      <div className="flex items-baseline justify-between mb-3">
        <div className="flex items-center gap-2">
          <span className="text-xs uppercase tracking-wider text-zinc-500">{label}</span>
          {saturated && (
            <span
              className="inline-flex items-center gap-1 rounded-full bg-red-500/15 text-red-700 dark:text-red-300 px-2 py-0.5 text-[10px] font-medium uppercase tracking-wider"
              title="At cap — additional spend is on Anthropic's pay-per-use Extra usage tier. Pct stops moving here."
            >
              <Zap className="size-3" />
              On extra usage
            </span>
          )}
        </div>
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

      {/* Headline: time-to-reset is always primary. */}
      <div className="mt-4">
        <Field label="Time to reset">
          {timeToResetMS !== undefined && timeToResetMS > 0 ? (
            <span className="tabular-nums">{fmtDuration(timeToResetMS)}</span>
          ) : (
            <span className="text-zinc-500">—</span>
          )}
          {resetTS && (
            <span className="text-zinc-500 ml-2">at {fmtAbs(resetTS)}</span>
          )}
        </Field>
      </div>

      {/* Warning row only when the burn-rate projection is actionable. */}
      {limitOK && limitETAMS !== undefined && (
        <div className="mt-2 flex items-center gap-1.5 text-amber-700 dark:text-amber-400 bg-amber-50 dark:bg-amber-950/40 border border-amber-200 dark:border-amber-900 rounded-md px-3 py-1.5 text-sm">
          <AlertTriangle className="size-3.5 shrink-0" />
          <span>
            On track to hit 100% in <strong className="tabular-nums">{fmtDuration(limitETAMS)}</strong>
            {limitETATS && (
              <span className="text-amber-600 dark:text-amber-500/80"> ({fmtAbs(limitETATS)})</span>
            )}
          </span>
        </div>
      )}

      <div className="mt-3 grid grid-cols-2 gap-3 text-xs">
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
