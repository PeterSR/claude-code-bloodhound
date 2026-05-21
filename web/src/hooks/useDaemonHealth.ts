import { useCallback, useEffect, useRef, useState } from 'react';

export type DaemonHealth = 'unknown' | 'up' | 'down';

export type DaemonHealthState = {
  health: DaemonHealth;
  /** Force an immediate probe (used by the "Check now" button). */
  check: () => void;
  /** Whether a probe request is currently in flight. */
  probing: boolean;
  /** Timestamp (ms) of the most recently completed probe; 0 before the first. */
  lastProbeAt: number;
  /** Polling cadence; the consumer can render an exact countdown to the next check. */
  intervalMs: number;
};

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
 * to the dashboard the moment the daemon comes up.
 */
export function useDaemonHealth(intervalMs = 2000): DaemonHealthState {
  const [health, setHealth] = useState<DaemonHealth>('unknown');
  const [lastProbeAt, setLastProbeAt] = useState(0);
  const [probing, setProbing] = useState(false);
  const probeRef = useRef<() => Promise<void>>(() => Promise.resolve());

  useEffect(() => {
    let cancelled = false;

    const probe = async () => {
      setProbing(true);
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
      } finally {
        if (!cancelled) {
          setLastProbeAt(Date.now());
          setProbing(false);
        }
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

  return { health, check, probing, lastProbeAt, intervalMs };
}
