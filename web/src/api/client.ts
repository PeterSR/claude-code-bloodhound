/**
 * Tiny fetch wrapper for the Bloodhound API.
 *
 * In dev (npm run dev), Vite reverse-proxies /api/* to the daemon's unix
 * socket at $XDG_RUNTIME_DIR/bloodhound/api.sock. In the production GUI
 * binary, Wails' AssetServer middleware does the same proxy.
 */

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

export async function apiGet<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`/api${path}`, {
    ...init,
    headers: { Accept: 'application/json', ...(init?.headers || {}) },
  });
  if (!res.ok) {
    const body = await res.text().catch(() => '');
    throw new ApiError(res.status, body || `${res.status} ${res.statusText}`);
  }
  return (await res.json()) as T;
}
