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

/**
 * A person who may sign in to this dokkup.
 *
 * There is no role beyond `isOwner`, and none should be read into it: every
 * operator is root-equivalent on the Dokku host, so `isOwner` records who
 * redeemed the setup token — which is what IP Mode restricts sign-in to — and
 * never a permission level the interface may grant or withhold.
 */
export interface Operator {
  email: string;
  name: string;
  isOwner: boolean;
}

export interface Session {
  mode: Mode;
  /** True when dokkup is reached by IP address and allows only the owner. */
  ownerOnly: boolean;
  authenticated: boolean;
  setupCompleted: boolean;
  /**
   * Present only when `authenticated` is true; the server omits it otherwise
   * rather than sending a null, so the absence is the signal.
   */
  operator?: Operator;
}

export interface Health {
  status: 'ok' | 'degraded';
  dokku?: string;
}

/**
 * What redeeming the setup token takes.
 *
 * The token is the credential the installer printed once. Send it and let it
 * go: never write it to localStorage or sessionStorage, never put it in a
 * query string, and never keep it anywhere that outlives the redemption. Both
 * storage and the URL survive the screen that collected it, and the URL also
 * ends up in a proxy's access log, in browser history and in a Referer header.
 */
export interface SetupRequest {
  token: string;
  email: string;
  name: string;
  password: string;
}

/**
 * The owner that redeeming created. No session comes back with it — signing in
 * is a separate act, so the screen that redeems must send the operator to sign
 * in rather than assume they already are.
 */
export interface SetupResult {
  operator: Operator;
}

/**
 * What signing in takes.
 *
 * Send the password and forget it: whoever builds the sign-in screen must not
 * keep it in a store, in a variable that outlives the submission, or in the
 * URL. Nothing needs it a second time — the session that comes back is the
 * only thing worth holding on to, and the browser already holds that as an
 * httpOnly cookie, out of script's reach. A password left in a store is
 * readable by injected script, and one left in a URL is written to history and
 * to every access log on the way.
 */
export interface SignInRequest {
  email: string;
  password: string;
}

/**
 * Who signed in. The session cookie itself never appears here, and there is
 * nothing to save: the browser stored it from `Set-Cookie` before this
 * resolved, so a caller re-reads `api.session()` rather than holding a token.
 */
export interface SignInResult {
  operator: Operator;
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

  // A 204 is an answer, not an empty one: the server says the thing happened
  // and has nothing to describe. Asking it for JSON would reject, and letting
  // that rejection fall into a catch would make a deliberate no-body
  // indistinguishable from a malformed one. So the status is read first and
  // the parse is never attempted. The catch that remains covers a body that
  // promised JSON and was not — an HTML error page from a proxy in front of
  // dokkup, say — and it leaves `body` null so the checks below still run.
  const body = response.status === 204 ? null : await response.json().catch(() => null);

  // A 503 on a read is a state to render rather than a failure to raise: it is
  // how /health reports that Dokku is unreachable, and the body carries which
  // dokkup answered. On a mutation it is a refusal — nothing happened — so it
  // must throw like any other, or a caller would take `{ error }` for a result.
  const degradedRead = response.status === 503 && method === 'GET';

  if (!response.ok && !degradedRead) {
    throw new ApiError(response.status, body?.error ?? response.statusText);
  }

  return body as T;
}

export const api = {
  session: () => request<Session>('/session'),
  health: () => request<Health>('/health'),

  /**
   * Redeem the setup token, creating the owner. The only mutation dokkup
   * accepts from someone who has not signed in; every failure arrives as an
   * ApiError whose message is meant to be shown as it is.
   */
  setup: (body: SetupRequest) =>
    request<SetupResult>('/setup', { method: 'POST', body: JSON.stringify(body) }),

  /**
   * Sign in. On success the browser has stored the session cookie by the time
   * this resolves; there is nothing for the caller to keep.
   *
   * Every refusal arrives as a 401 carrying one deliberately unhelpful
   * message, and it must be shown exactly as the server wrote it. An unknown
   * email address, a wrong password and a non-Owner in IP Mode are one answer
   * on purpose: telling them apart would confirm to an attacker that an
   * address exists, or that the credentials were in fact right and only the
   * mode stood in the way. Do not branch on the mode or on `ownerOnly` here to
   * say something friendlier — that would undo the server's care from the
   * outside. The audit trail is where the real reason is recorded.
   */
  signIn: (body: SignInRequest) =>
    request<SignInResult>('/signin', { method: 'POST', body: JSON.stringify(body) }),

  /**
   * Sign out, which revokes the session on the server rather than merely
   * dropping a cookie the client could not have dropped anyway: the cookie is
   * httpOnly, and a session forgotten only by the client is still a session.
   *
   * Answers 204 with no body. `request` yields null for a body-less response,
   * so this awaits and returns nothing of its own rather than passing that
   * null off as a `void`: the promise resolves to undefined, which is what the
   * type says. Made explicit because the two disagreed silently, and nothing
   * in the frontend gate would have caught it.
   */
  signOut: async (): Promise<void> => {
    await request<null>('/signout', { method: 'POST' });
  }
};
