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

/**
 * POST JSON and read JSON back.
 *
 * The API answers a rejected write with 400 and a human-readable `error`
 * string rather than a code, because the messages come from validation
 * written for a person (`a budget needs a spend rule, a meter rule, or
 * both`). Surfacing that text is the whole point, so it is lifted out of the
 * body rather than replaced with the status line.
 */
export async function apiPost<T>(path: string, body: unknown): Promise<T> {
  const res = await fetch(`/api${path}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
    body: JSON.stringify(body),
  });
  const text = await res.text();
  let parsed: unknown = null;
  try {
    parsed = text ? JSON.parse(text) : null;
  } catch {
    // Fall through: a non-JSON body means the message is the text itself.
  }
  if (!res.ok) {
    const msg =
      (parsed as { error?: string } | null)?.error || text || `${res.status} ${res.statusText}`;
    throw new ApiError(res.status, msg);
  }
  return parsed as T;
}
