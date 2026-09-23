import { useSyncExternalStore } from 'react';

/**
 * The Claude account the whole app is looking at.
 *
 * Every account has its own /usage meter, windows and attribution, so every
 * usage read carries `?account=`. null means "let the daemon pick", which is
 * the primary account: whoever the first watched config dir is logged in to.
 * Persisted so the choice survives a reload; a stale id (an account merged
 * away) is dropped by the sidebar picker once /api/accounts says so.
 */

const STORAGE_KEY = 'bloodhound.account';

function load(): number | null {
  try {
    const v = Number(localStorage.getItem(STORAGE_KEY));
    return Number.isInteger(v) && v > 0 ? v : null;
  } catch {
    return null;
  }
}

let current: number | null = load();
const listeners = new Set<() => void>();

export function getAccount(): number | null {
  return current;
}

export function setAccount(id: number | null) {
  if (id === current) return;
  current = id;
  try {
    if (id === null) localStorage.removeItem(STORAGE_KEY);
    else localStorage.setItem(STORAGE_KEY, String(id));
  } catch {
    // Storage unavailable: the choice just lasts until reload.
  }
  listeners.forEach((l) => l());
}

function subscribe(l: () => void) {
  listeners.add(l);
  return () => listeners.delete(l);
}

/** The selected account id, re-rendering when it changes. */
export function useAccount(): number | null {
  return useSyncExternalStore(subscribe, getAccount);
}

/** Appends the selected account to an API path. The account list itself is
 *  the one read that must not be scoped. */
export function withAccount(path: string): string {
  if (current === null || path.startsWith('/accounts')) return path;
  return path + (path.includes('?') ? '&' : '?') + 'account=' + current;
}

export type AccountInfo = {
  id: number;
  name: string;
  label?: string;
  email?: string;
  org_name?: string;
  rate_limit_tier?: string;
  metered: boolean;
  config_dirs?: string[];
  last_seen_ms?: number;
};

export type AccountsResponse = {
  ok: boolean;
  primary_id: number;
  accounts: AccountInfo[];
};
