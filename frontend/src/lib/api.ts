export type Page = {
  limit: number;
  offset: number;
  has_more: boolean;
  next_offset: number | null;
  prev_offset: number | null;
};

export type Envelope<T> = { data: T; page?: Page; request_id: string };

export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
    readonly requestId: string,
    readonly fields?: Record<string, string>,
  ) {
    super(message);
    this.name = 'ApiError';
  }
}

export class NetworkError extends Error {
  constructor() {
    super('Unable to reach the server. Check your connection.');
    this.name = 'NetworkError';
  }
}

export class ProtocolError extends Error {
  constructor(readonly status: number, readonly requestId: string) {
    super('The server returned an invalid response.');
    this.name = 'ProtocolError';
  }
}

type Options = {
  method?: 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE';
  body?: unknown;
  csrfToken?: string;
  signal?: AbortSignal;
};

export async function apiFetch<T>(path: string, options: Options = {}): Promise<Envelope<T>> {
  if (!path.startsWith('/api/') || path.includes('\\') || !new URL(path, 'http://localhost').pathname.startsWith('/api/')) {
    throw new Error('Invalid API path.');
  }
  const { method = 'GET', body, csrfToken, signal } = options;
  const headers: Record<string, string> = { Accept: 'application/json' };
  if (method !== 'GET') {
    if (!csrfToken || !/^[A-Za-z0-9_-]{43}$/.test(csrfToken)) throw new Error('Open a new form and try again.');
    headers['X-CSRF-Token'] = csrfToken;
    headers['Content-Type'] = 'application/json';
  }
  let response: Response;
  const encodedBody = method === 'GET' ? undefined : JSON.stringify(body ?? {});
  try {
    response = await fetch(path, { method, headers, body: encodedBody, signal, credentials: 'same-origin', redirect: 'error' });
  } catch (error) {
    if (signal?.aborted) throw error;
    throw new NetworkError();
  }
  const requestId = response.headers.get('X-Request-ID') ?? '';
  if (!/^application\/json(?:;|$)/i.test(response.headers.get('Content-Type') ?? '')) {
    throw new ProtocolError(response.status, requestId);
  }
  let result;
  try {
    result = await response.json();
  } catch (error) {
    if (signal?.aborted) throw error;
    if (error instanceof SyntaxError) throw new ProtocolError(response.status, requestId);
    throw new NetworkError();
  }
  if (!result || typeof result !== 'object' || typeof result.request_id !== 'string') {
    throw new ProtocolError(response.status, requestId);
  }
  if (!response.ok) {
    if ('data' in result || typeof result.error?.code !== 'string' || typeof result.error?.message !== 'string') {
      throw new ProtocolError(response.status, requestId);
    }
    throw new ApiError(response.status, result.error.code, result.error.message, result.request_id, result.error.fields);
  }
  if (!('data' in result) || 'error' in result) throw new ProtocolError(response.status, requestId);
  return result as Envelope<T>;
}
