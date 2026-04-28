import { Settings as SettingsIcon } from 'lucide-react';

export default function Settings() {
  return (
    <div>
      <div className="flex items-center gap-2 mb-2">
        <SettingsIcon className="size-5 text-zinc-500" />
        <h1 className="text-2xl font-semibold tracking-tight">Settings</h1>
      </div>
      <p className="text-zinc-600 dark:text-zinc-400 max-w-2xl">
        Plan tier (cosmetic — used only as community-insights metadata), poll
        intervals, port, copy-paste hook and statusline snippets, and the
        opt-in <em>Join the pack</em> toggle for community insights.
      </p>
      <p className="mt-8 text-sm text-zinc-500">
        Backend API not wired yet. The page renders {`{`}config.json{`}`} once <code className="font-mono text-xs">/api/settings</code> ships.
      </p>
    </div>
  );
}
