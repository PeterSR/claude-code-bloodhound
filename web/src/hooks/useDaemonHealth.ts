import { useCallback, useEffect, useRef, useState } from 'react';

export type DaemonHealth = 'unknown' | 'up' | 'down';

/**
 * useDaemonHealth probes /api/health and reports whether the daemon is
 * reachable. Used by the top-level gate to decide between rendering the
 * dashboard and the setup wizard.
 *
 * When the daemon is down, the proxy returns 502 or the request fails
 * with a TypeError ("Failed to fetch") in dev (Vite proxy ECONNREFUSED).
 * Either signal flips state to 'down'.
 *
 * Polls on `intervalMs` regardless of state so the wizard auto-switches
 * to the dashboard the moment the daemon comes up. The returned `check`
 * fires an immediate probe (used by the "Check now" button).
 */
export function useDaemonHealth(intervalMs = 2000): {
  health: DaemonHealth;
  check: () => void;
} {
  const [health, setHealth] = useState<DaemonHealth>('unknown');
  const probeRef = useRef<() => Promise<void>>(() => Promise.resolve());

  useEffect(() => {
    let cancelled = false;

    const probe = async () => {
      try {
        const res = await fetch('/api/health', {
          headers: { Accept: 'application/json' },
          cache: 'no-store',
        });
        if (cancelled) return;
        setHealth(res.ok ? 'up' : 'down');
      } catch {
        if (cancelled) return;
        setHealth('down');
      }
    };
    probeRef.current = probe;

    void probe();
    const id = window.setInterval(probe, intervalMs);
    return () => {
      cancelled = true;
      window.clearInterval(id);
    };
  }, [intervalMs]);

  const check = useCallback(() => {
    void probeRef.current();
  }, []);

  return { health, check };
}
