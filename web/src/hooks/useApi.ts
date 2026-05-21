import { useEffect, useState } from 'react';
import { apiGet, ApiError } from '../api/client';

type ApiState<T> = {
  data: T | null;
  error: Error | null;
  /** True only when there's no data yet (initial load). Use this to gate
   *  first-render placeholders — never flips back to true once data
   *  arrives, so polls and filter changes don't unmount the page. */
  loading: boolean;
  /** True whenever a fetch is in-flight (initial, polled, or manual).
   *  Drives the ReloadButton spin so users can see auto-refreshes happen. */
  refreshing: boolean;
  refresh: () => void;
};

export function useApi<T>(path: string, refreshIntervalMs?: number): ApiState<T> {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<Error | null>(null);
  const [refreshing, setRefreshing] = useState(true);
  const [tick, setTick] = useState(0);

  useEffect(() => {
    let cancelled = false;

    const run = async () => {
      // Floor the spinner-visible time at ~300ms so users can see polls
      // happen even when the unix-socket round-trip completes in <16ms.
      const MIN_SPIN_MS = 300;
      const started = Date.now();
      if (!cancelled) setRefreshing(true);
      try {
        const json = await apiGet<T>(path);
        if (!cancelled) {
          setData(json);
          setError(null);
        }
      } catch (e) {
        if (!cancelled) {
          setError(e instanceof ApiError ? e : (e as Error));
        }
      } finally {
        const left = MIN_SPIN_MS - (Date.now() - started);
        if (left > 0) await new Promise((r) => setTimeout(r, left));
        if (!cancelled) setRefreshing(false);
      }
    };

    run();

    let interval: number | undefined;
    if (refreshIntervalMs && refreshIntervalMs > 0) {
      interval = window.setInterval(run, refreshIntervalMs);
    }
    return () => {
      cancelled = true;
      if (interval) window.clearInterval(interval);
    };
  }, [path, refreshIntervalMs, tick]);

  return {
    data,
    error,
    loading: data === null && error === null,
    refreshing,
    refresh: () => setTick((t) => t + 1),
  };
}
