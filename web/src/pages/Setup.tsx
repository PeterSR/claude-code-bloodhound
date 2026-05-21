import { useEffect, useState } from 'react';
import { Dog, Copy, Check, Loader2 } from 'lucide-react';
import type { DaemonHealthState } from '../hooks/useDaemonHealth';

/**
 * SetupWizard renders when /api/health is unreachable. Shown instead of
 * the dashboard chrome — the daemon must be up before anything else in
 * the app is meaningful.
 *
 * Polling for daemon-up happens one level above (DaemonGate in App.tsx);
 * this view shows when the next probe will fire and lets the user trigger
 * one early via "Check now".
 */
export default function SetupWizard({ daemon }: { daemon: DaemonHealthState }) {
  return (
    <div className="min-h-screen flex items-center justify-center bg-zinc-50 dark:bg-zinc-950 text-zinc-900 dark:text-zinc-100 p-6">
      <div className="max-w-2xl w-full space-y-6">
        <div className="flex items-center gap-3">
          <Dog className="size-6 text-rose-500" />
          <h1 className="text-2xl font-semibold tracking-tight">Bloodhound</h1>
        </div>

        <div className="rounded-lg border border-zinc-200 dark:border-zinc-800 bg-white dark:bg-zinc-900 p-6 space-y-4">
          <div>
            <h2 className="text-lg font-medium">The daemon isn't running</h2>
            <p className="text-sm text-zinc-600 dark:text-zinc-400 mt-1">
              This window is just a viewer. The <code className="px-1 py-0.5 rounded bg-zinc-100 dark:bg-zinc-800 text-xs">bloodhound</code> daemon
              is what polls <code className="px-1 py-0.5 rounded bg-zinc-100 dark:bg-zinc-800 text-xs">/usage</code>, ingests session files, and
              serves the API the dashboard reads from. Start it and this page will
              switch over automatically.
            </p>
          </div>

          <Step
            n={1}
            title="Try the quickest path"
            hint="One foreground process. Closes when you Ctrl-C or close the terminal."
            cmd="bloodhound daemon"
          />

          <Step
            n={2}
            title="Or run it persistently via systemd"
            hint="Survives reboots. Recommended if you keep Claude Code open often. Writes ~/.config/systemd/user/bloodhound.service pointing at the binary you ran it with."
            cmd="bloodhound install --enable"
          />

          <Details summary="Don't have bloodhound on $PATH?">
            <p className="text-sm text-zinc-600 dark:text-zinc-400">
              You'll need the Go toolchain. From a clone of the repo:
            </p>
            <CommandBlock cmd="make install" />
            <p className="text-sm text-zinc-600 dark:text-zinc-400">
              A polished release artifact (no Go required) is on the roadmap;
              for now this is a contributor flow.
            </p>
          </Details>
        </div>

        <div className="flex items-center justify-between text-sm">
          <ProbeStatus daemon={daemon} />
          <button
            onClick={daemon.check}
            disabled={daemon.probing}
            className="px-3 py-1.5 rounded-md border border-zinc-300 dark:border-zinc-700 text-sm hover:bg-zinc-100 dark:hover:bg-zinc-900 transition disabled:opacity-50 disabled:cursor-not-allowed"
          >
            Check now
          </button>
        </div>
      </div>
    </div>
  );
}

function ProbeStatus({ daemon }: { daemon: DaemonHealthState }) {
  // Re-render on a 250ms tick so the countdown is smooth without spamming
  // the rest of the tree. Source of truth stays in the hook.
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const id = window.setInterval(() => setNow(Date.now()), 250);
    return () => window.clearInterval(id);
  }, []);

  if (daemon.probing || daemon.lastProbeAt === 0) {
    return (
      <div className="flex items-center gap-2 text-zinc-500">
        <Loader2 className="size-3.5 animate-spin" />
        Checking…
      </div>
    );
  }

  const remainingMs = daemon.lastProbeAt + daemon.intervalMs - now;
  const remainingS = Math.max(0, Math.ceil(remainingMs / 1000));
  return (
    <div className="flex items-center gap-2 text-zinc-500">
      <Loader2 className="size-3.5 animate-spin" />
      Checking in {remainingS}s…
    </div>
  );
}

function Step({
  n,
  title,
  hint,
  cmd,
}: {
  n: number;
  title: string;
  hint?: string;
  cmd: string;
}) {
  return (
    <div className="space-y-2">
      <div className="flex items-center gap-2">
        <span className="size-5 rounded-full bg-zinc-200 dark:bg-zinc-800 text-xs font-medium flex items-center justify-center">
          {n}
        </span>
        <span className="font-medium text-sm">{title}</span>
      </div>
      {hint && (
        <p className="text-xs text-zinc-500 ml-7">{hint}</p>
      )}
      <div className="ml-7">
        <CommandBlock cmd={cmd} />
      </div>
    </div>
  );
}

function CommandBlock({ cmd }: { cmd: string }) {
  const [copied, setCopied] = useState(false);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(cmd);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1500);
    } catch {
      // Clipboard API can fail in non-secure contexts; silent no-op is
      // fine because the user can still select-copy the text manually.
    }
  };
  return (
    <div className="relative group">
      <pre className="rounded-md bg-zinc-100 dark:bg-zinc-800 text-xs font-mono px-3 py-2 pr-10 text-zinc-800 dark:text-zinc-200 overflow-x-auto whitespace-nowrap">
        {cmd}
      </pre>
      <button
        onClick={copy}
        title="Copy"
        className="absolute top-1.5 right-1.5 p-1 rounded hover:bg-zinc-200 dark:hover:bg-zinc-700 transition text-zinc-500"
      >
        {copied ? <Check className="size-3.5 text-emerald-500" /> : <Copy className="size-3.5" />}
      </button>
    </div>
  );
}

function Details({ summary, children }: { summary: string; children: React.ReactNode }) {
  return (
    <details className="group rounded-md border border-zinc-200 dark:border-zinc-800 px-3 py-2">
      <summary className="cursor-pointer text-sm font-medium text-zinc-700 dark:text-zinc-300 select-none">
        {summary}
      </summary>
      <div className="mt-2 space-y-2">{children}</div>
    </details>
  );
}

