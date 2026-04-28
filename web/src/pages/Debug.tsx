import { Bug } from 'lucide-react';

export default function Debug() {
  return (
    <div>
      <div className="flex items-center gap-2 mb-2">
        <Bug className="size-5 text-amber-500" />
        <h1 className="text-2xl font-semibold tracking-tight">Debug</h1>
      </div>
      <p className="text-zinc-600 dark:text-zinc-400 max-w-2xl">
        Last <code className="font-mono text-xs">/usage</code> raw dump, parser output, ingest stats,
        statusline preview, schema version. The is-this-thing-on page.
      </p>
      <p className="mt-8 text-sm text-zinc-500">
        Backend API not wired yet. Pieces land alongside the other pages.
      </p>
    </div>
  );
}
