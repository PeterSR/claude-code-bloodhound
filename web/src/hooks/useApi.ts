import { useEffect, useState } from 'react';
import { apiGet, ApiError } from '../api/client';

type ApiState<T> = {
  data: T | null;
  error: Error | null;
  loading: boolean;
  refresh: () => void;
};

/**
 * useApi fetches `path` (relative to /api) on mount, optionally polls on an
 * interval, and exposes a `refresh()` for manual reloads. StrictMode-safe:
 * an in-flight request from an unmounted hook is ignored.
 */
export function useApi<T>(path: string, refreshIntervalMs?: number): ApiState<T> {
  const [data, setData] = useState<T | null>(null);
  const [error, setError] = useState<Error | null>(null);
  const [loading, setLoading] = useState(true);
  const [tick, setTick] = useState(0);

  useEffect(() => {
    let cancelled = false;

    const run = async () => {
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
        if (!cancelled) setLoading(false);
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
    loading,
    refresh: () => setTick((t) => t + 1),
  };
}
