/**
 * The single place the frontend talks to the API.
 *
 * Every mutation carries a custom header. A browser will not attach one to a
 * cross-origin request without a successful preflight, which is what makes it
 * effective against cross-site request forgery alongside the SameSite session
 * cookie. Because the frontend is client-rendered there are no SvelteKit form
 * actions to inherit protection from, so this is the only mechanism.
 */
const CSRF_HEADER = 'X-Dokkup-CSRF';

export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string
  ) {
    super(message);
    this.name = 'ApiError';
  }
}

export type Mode = 'published' | 'ip';

export interface Session {
  mode: Mode;
  /** True when dokkup is reached by IP address and allows only the owner. */
  ownerOnly: boolean;
  authenticated: boolean;
  setupCompleted: boolean;
}

export interface Health {
  status: 'ok' | 'degraded';
  dokku?: string;
}

async function request<T>(path: string, init: RequestInit = {}): Promise<T> {
  const method = init.method ?? 'GET';
  const headers = new Headers(init.headers);

  if (!['GET', 'HEAD', 'OPTIONS'].includes(method)) {
    headers.set(CSRF_HEADER, '1');
    headers.set('Content-Type', 'application/json');
  }

  const response = await fetch(`/api${path}`, {
    ...init,
    headers,
    // Sessions are carried by a cookie, never by a token in storage: script
    // injected into the page can read storage, and cannot read an httpOnly
    // cookie.
    credentials: 'same-origin'
  });

  const body = await response.json().catch(() => null);

  if (!response.ok && response.status !== 503) {
    throw new ApiError(response.status, body?.error ?? response.statusText);
  }

  return body as T;
}

export const api = {
  session: () => request<Session>('/session'),
  health: () => request<Health>('/health')
};
