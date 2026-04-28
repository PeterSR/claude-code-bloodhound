import { Activity } from 'lucide-react';

export default function Now() {
  return (
    <div>
      <div className="flex items-center gap-2 mb-2">
        <Activity className="size-5 text-rose-500" />
        <h1 className="text-2xl font-semibold tracking-tight">Now</h1>
      </div>
      <p className="text-zinc-600 dark:text-zinc-400 max-w-2xl">
        Where you stand right now: session %, week %, projected ETA, and an
        estimate of what your next prompt will cost.
      </p>
      <div className="mt-8 grid gap-4 grid-cols-1 md:grid-cols-2 max-w-3xl">
        <PlaceholderCard label="Session (5h)" hint="ring + ETA" />
        <PlaceholderCard label="Week" hint="ring + ETA" />
        <PlaceholderCard label="Next prompt" hint="estimated %" />
        <PlaceholderCard label="Cache nudge" hint="reply within Xm" />
      </div>
      <p className="mt-8 text-sm text-zinc-500">
        Backend API not wired yet. Cards will populate once <code className="font-mono text-xs">/api/now</code> ships.
      </p>
    </div>
  );
}

function PlaceholderCard({ label, hint }: { label: string; hint: string }) {
  return (
    <div className="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-5">
      <div className="text-xs uppercase tracking-wider text-zinc-500">{label}</div>
      <div className="mt-3 text-2xl font-semibold tabular-nums">—</div>
      <div className="mt-1 text-xs text-zinc-500">{hint}</div>
    </div>
  );
}
