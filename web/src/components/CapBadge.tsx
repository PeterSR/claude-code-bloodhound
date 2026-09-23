import { AlertCircle, ArrowDown, CircleSlash, Zap } from 'lucide-react';
import { fmtAbs } from '../lib/format';

/** The values NowWindow.cap_state takes (see internal/api/routes/now.go). */
export type CapState = 'extra_usage' | 'refused' | 'low_priority';

export type QuotaState = {
  refused: boolean;
  refused_ts?: string;
  bucket?: string;
  reset_ts?: string;
  overage_status?: string;
  overage_disabled_reason?: string;
  using_overage: boolean;
  low_priority_offered: boolean;
  low_priority_active: boolean;
  low_priority_since_ts?: string;
  low_priority_until_ts?: string;
  low_priority_sessions?: string[];
};

type Props = {
  saturated?: boolean;
  capState?: CapState | string;
  quota?: QuotaState | null;
};

/**
 * CapBadge says what a window sitting at its cap actually means.
 *
 * The gauge used to render one badge here, unconditionally, reading "On
 * extra usage". That is one of three things a pinned meter can mean and the
 * wrong one whenever the pay-per-use tier is not available — which is the
 * common case, since a plan without credits on it is refused rather than
 * billed. The badge was therefore telling users money was being spent at
 * exactly the moments none was.
 *
 * The three readings, and why each needs its own treatment:
 *
 *   Extra usage — spend continues past the cap and is billed. Red, because
 *   the meter has stopped being a warning and the spend has not.
 *
 *   Refused — nothing is absorbing the overflow and requests are turned
 *   away. Red, because work stops here.
 *
 *   Low priority — the window is closed but requests are still going
 *   through, slower, against the weekly allowance. Deliberately NOT red.
 *   Someone who reads red stops working, and there is nothing here to stop
 *   for; the only thing they need to know is that it is slower now and that
 *   it is the weekly limit paying for it.
 *
 * With no evidence either way the badge still appears but claims nothing —
 * "At cap" and an honest tooltip beats a confident guess.
 */
export default function CapBadge({ saturated, capState, quota }: Props) {
  if (!saturated && !capState) return null;

  const until = quota?.low_priority_until_ts ?? quota?.reset_ts;
  const resets = until ? ` It reopens at ${fmtAbs(until)}.` : '';

  if (capState === 'low_priority') {
    return (
      <Badge
        tone="sky"
        icon={<ArrowDown className="size-3" />}
        label="Low priority"
        title={
          'The window is closed, but requests are still going through at a lower priority.' +
          resets +
          ' They may wait for spare capacity, and they draw on the weekly limit rather than this one.'
        }
      />
    );
  }

  if (capState === 'extra_usage') {
    return (
      <Badge
        tone="red"
        icon={<Zap className="size-3" />}
        label="On extra usage"
        title={
          "At cap — additional spend is on Anthropic's pay-per-use Extra usage tier. Pct stops moving here." +
          resets
        }
      />
    );
  }

  if (capState === 'refused') {
    const why = quota?.overage_disabled_reason
      ? ` Extra usage is unavailable (${quota.overage_disabled_reason.replace(/_/g, ' ')}).`
      : '';
    const offer = quota?.low_priority_offered
      ? ' Claude Code can carry on at a lower priority instead of waiting.'
      : '';
    return (
      <Badge
        tone="red"
        icon={<AlertCircle className="size-3" />}
        label="Limit reached"
        title={'At cap, and requests are being refused.' + why + resets + offer}
      />
    );
  }

  return (
    <Badge
      tone="zinc"
      icon={<CircleSlash className="size-3" />}
      label="At cap"
      title="Pinned at the cap, so the percentage has stopped moving. No refused request has been seen in this window, so bloodhound cannot say whether spend is billing to extra usage, being refused, or running at low priority."
    />
  );
}

const tones = {
  red: 'bg-red-500/15 text-red-700 dark:text-red-300',
  sky: 'bg-sky-500/15 text-sky-700 dark:text-sky-300',
  zinc: 'bg-zinc-500/15 text-zinc-600 dark:text-zinc-400',
} as const;

function Badge({
  tone,
  icon,
  label,
  title,
}: {
  tone: keyof typeof tones;
  icon: React.ReactNode;
  label: string;
  title: string;
}) {
  return (
    <span
      className={`inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-[10px] font-medium uppercase tracking-wider ${tones[tone]}`}
      title={title}
    >
      {icon}
      {label}
    </span>
  );
}
