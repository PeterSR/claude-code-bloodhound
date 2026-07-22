// Formatting helpers shared across pages.

export function fmtNumber(n: number | bigint): string {
  if (typeof n === 'bigint') return n.toLocaleString();
  if (n >= 1e9) return (n / 1e9).toFixed(2) + 'B';
  if (n >= 1e6) return (n / 1e6).toFixed(2) + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'k';
  return Math.round(n).toString();
}

/** An exact count (turns, sessions, windows, compactions), thousands
 *  grouped with a literal comma, pinned to the "en-US" locale rather than
 *  `toLocaleString()`'s default of whatever the viewer's own locale is.
 *  fmtNumber above already spells its decimal point as '.' ("237.67M"); a
 *  locale that also uses '.' as its own thousands separator (as on this
 *  machine) turns an exact count like 4337 into "4.337", which reads as a
 *  fraction sitting next to a token figure that uses the same character
 *  for something else entirely. A comma can never collide with that
 *  decimal point. (A narrow no-break space would dodge the collision too,
 *  but disappears outright in some fonts, so comma it is.)
 */
export function fmtCount(n: number): string {
  return Math.round(n).toLocaleString('en-US');
}

/** Friendly absolute display: "Apr 29, 7:30 AM". */
export function fmtAbs(iso: string): string {
  if (!iso) return '—';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString(undefined, {
    month: 'short',
    day: 'numeric',
    hour: 'numeric',
    minute: '2-digit',
  });
}

/** "8m ago", "1.2h ago", "3d ago". */
export function fmtRel(seconds: number): string {
  const s = Math.max(0, Math.round(seconds));
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.round(s / 60)}m ago`;
  if (s < 86400) return `${(s / 3600).toFixed(1)}h ago`;
  return `${Math.round(s / 86400)}d ago`;
}

/** ms duration → "2h 15m" / "12m" / "8s". */
export function fmtDuration(ms: number): string {
  if (ms <= 0) return '0s';
  const totalSec = Math.round(ms / 1000);
  if (totalSec < 60) return `${totalSec}s`;
  const min = Math.floor(totalSec / 60);
  if (min < 60) return `${min}m`;
  const h = Math.floor(min / 60);
  const remMin = min % 60;
  if (h < 24) return remMin === 0 ? `${h}h` : `${h}h ${remMin}m`;
  const d = Math.floor(h / 24);
  const remH = h % 24;
  return remH === 0 ? `${d}d` : `${d}d ${remH}h`;
}

/** A pct severity color: green/yellow/orange/red as you climb. */
export function pctColor(pct: number): string {
  if (pct >= 90) return 'text-red-500';
  if (pct >= 70) return 'text-orange-500';
  if (pct >= 40) return 'text-amber-500';
  return 'text-emerald-500';
}

export function pctBgColor(pct: number): string {
  if (pct >= 90) return 'bg-red-500';
  if (pct >= 70) return 'bg-orange-500';
  if (pct >= 40) return 'bg-amber-500';
  return 'bg-emerald-500';
}
