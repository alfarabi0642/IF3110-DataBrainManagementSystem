import { afterEach, describe, expect, it, vi } from 'vitest';
import { apiFetch } from './api';
import { createQueryClient, refreshSession, type Session } from './session';

const clients: ReturnType<typeof createQueryClient>[] = [];
afterEach(() => { clients.forEach((client) => client.clear()); clients.length = 0; vi.unstubAllGlobals(); });
function client() {
  const value = createQueryClient();
  clients.push(value);
  return value;
}
function session(state: Session['state'] = 'authenticated', family = 'a', user = 'alice'): Session {
  return { state, csrf_token: family.repeat(43), absolute_expires_at: '2026-11-06T20:00:00Z',
    user: state === 'anonymous' ? null : { id: user, username: user, display_name: user, is_platform_admin: false } };
}
function response(data: unknown, status = 200) {
  return new Response(JSON.stringify(status === 200 ? { data, request_id: 'r' } : { error: data, request_id: 'r' }), {
    status, headers: { 'Content-Type': 'application/json' },
  });
}

describe('session and protected cache', () => {
  it.each([
    session('authenticated', 'b'), session('authenticated', 'a', 'bob'),
    session('reauth_required'), session('anonymous'),
  ])('clears every protected resource after a family/identity/state change: %j', async (next) => {
    const queryClient = client();
    queryClient.setQueryData(['session'], { data: session(), request_id: 'r' });
    queryClient.setQueryData(['databases'], { private: true });
    queryClient.setQueryData(['admin', 'users'], { private: true });
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response(next)));
    await refreshSession(queryClient);
    expect(queryClient.getQueryData(['databases'])).toBeUndefined();
    expect(queryClient.getQueryData(['admin', 'users'])).toBeUndefined();
    expect(queryClient.getQueryData(['session'])).toMatchObject({ data: next });
  });

  it('keeps protected data when the same family remains authenticated', async () => {
    const queryClient = client();
    queryClient.setQueryData(['session'], { data: session(), request_id: 'r' });
    queryClient.setQueryData(['databases'], ['School']);
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response(session())));
    await refreshSession(queryClient);
    expect(queryClient.getQueryData(['databases'])).toEqual(['School']);
  });

  it('cancels in-flight protected reads before replacing the family', async () => {
    const queryClient = client();
    queryClient.setQueryData(['session'], { data: session(), request_id: 'r' });
    let signal: AbortSignal | undefined;
    const pending = queryClient.fetchQuery({ queryKey: ['history'], queryFn: (context) => {
      signal = context.signal;
      return new Promise(() => {});
    } }).catch(() => {});
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response(session('authenticated', 'b'))));
    await refreshSession(queryClient);
    await pending;
    expect(signal?.aborted).toBe(true);
    expect(queryClient.getQueryCache().find({ queryKey: ['history'] })).toBeUndefined();
  });

  it.each([[401, 'SESSION_INVALID'], [401, 'REAUTH_REQUIRED'], [403, 'CSRF_INVALID']])('bootstraps after %s %s without replaying a mutation', async (status, code) => {
    const queryClient = client();
    queryClient.setQueryData(['databases'], ['private']);
    const fetch = vi.fn().mockResolvedValueOnce(response({ code, message: 'Open a new form and try again.' }, status as number))
      .mockResolvedValueOnce(response(session('anonymous', 'b')));
    vi.stubGlobal('fetch', fetch);
    const mutation = queryClient.getMutationCache().build(queryClient, {
      mutationFn: () => apiFetch('/api/logout', { method: 'POST', csrfToken: 'a'.repeat(43) }),
    });
    await expect(mutation.execute(undefined)).rejects.toMatchObject({ code });
    expect(fetch.mock.calls.map(([path]) => path)).toEqual(['/api/logout', '/api/session']);
    expect(queryClient.getQueryData(['databases'])).toBeUndefined();
  });

  it('keeps a failed login local and does not retry credentials', async () => {
    const queryClient = client();
    const fetch = vi.fn().mockResolvedValue(response({ code: 'INVALID_CREDENTIALS', message: 'Invalid username or password.' }, 401));
    vi.stubGlobal('fetch', fetch);
    const mutation = queryClient.getMutationCache().build(queryClient, {
      mutationFn: () => apiFetch('/api/login', { method: 'POST', csrfToken: 'a'.repeat(43), body: { username: 'alice', password: 'wrong' } }),
    });
    await expect(mutation.execute(undefined)).rejects.toMatchObject({ code: 'INVALID_CREDENTIALS' });
    expect(fetch).toHaveBeenCalledTimes(1);
  });

  it('clears protected data after successful logout even if bootstrap is unavailable', async () => {
    const queryClient = client();
    queryClient.setQueryData(['databases'], ['private']);
    const fetch = vi.fn().mockResolvedValueOnce(response({})).mockRejectedValueOnce(new TypeError('offline'));
    vi.stubGlobal('fetch', fetch);
    const mutation = queryClient.getMutationCache().build(queryClient, {
      meta: { auth: true }, mutationFn: () => apiFetch('/api/logout', { method: 'POST', csrfToken: 'a'.repeat(43) }),
    });
    await expect(mutation.execute(undefined)).resolves.toMatchObject({ data: {} });
    expect(queryClient.getQueryData(['databases'])).toBeUndefined();
    expect(queryClient.getQueryCache().find({ queryKey: ['session'] })?.state.error).toMatchObject({ name: 'NetworkError' });
    expect(queryClient.getMutationCache().getAll()).toHaveLength(0);
    expect(fetch.mock.calls.map(([path]) => path)).toEqual(['/api/logout', '/api/session']);
  });

  it('checks bootstrap after a lost auth response and never retries the mutation', async () => {
    const queryClient = client();
    queryClient.setQueryData(['databases'], ['private']);
    const fetch = vi.fn().mockRejectedValueOnce(new TypeError('connection lost')).mockResolvedValueOnce(response(session('anonymous', 'b')));
    vi.stubGlobal('fetch', fetch);
    const mutation = queryClient.getMutationCache().build(queryClient, {
      meta: { auth: true }, mutationFn: () => apiFetch('/api/logout', { method: 'POST', csrfToken: 'a'.repeat(43) }),
    });
    await expect(mutation.execute(undefined)).rejects.toMatchObject({ name: 'NetworkError' });
    expect(queryClient.getQueryData(['databases'])).toBeUndefined();
    expect(queryClient.getQueryData(['session'])).toMatchObject({ data: { state: 'anonymous' } });
    expect(fetch.mock.calls.map(([path]) => path)).toEqual(['/api/logout', '/api/session']);
  });
});
