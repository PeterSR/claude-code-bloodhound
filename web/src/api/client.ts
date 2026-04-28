/**
 * Tiny fetch wrapper for the Bloodhound API.
 *
 * In dev (npm run dev), Vite proxies /api/* to the Go server on :7777.
 * In a built bundle served by `bloodhound serve`, /api/* is same-origin.
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
