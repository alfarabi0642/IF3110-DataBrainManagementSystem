import { afterEach, describe, expect, it, vi } from 'vitest';
import { apiFetch, ApiError, NetworkError, ProtocolError } from './api';

afterEach(() => vi.unstubAllGlobals());

function respond(body: unknown, status = 200, contentType = 'application/json') {
  const fetch = vi.fn().mockResolvedValue(new Response(JSON.stringify(body), {
    status, headers: { 'Content-Type': contentType, 'X-Request-ID': 'request-1' },
  }));
  vi.stubGlobal('fetch', fetch);
  return fetch;
}

describe('JSON transport', () => {
  it('reads data and pagination with a same-origin cookie', async () => {
    const envelope = { data: [], page: { limit: 50, offset: 0, has_more: false, next_offset: null, prev_offset: null }, request_id: 'request-1' };
    const fetch = respond(envelope);
    expect(await apiFetch('/api/databases')).toEqual(envelope);
    expect(fetch).toHaveBeenCalledWith('/api/databases', expect.objectContaining({ credentials: 'same-origin', redirect: 'error' }));
  });

  it('carries CSRF in the header and JSON in the body', async () => {
    const fetch = respond({ data: {}, request_id: 'request-1' });
    await apiFetch('/api/logout', { method: 'POST', csrfToken: 'a'.repeat(43), body: {} });
    expect(fetch).toHaveBeenCalledWith('/api/logout', expect.objectContaining({
      body: '{}', headers: { Accept: 'application/json', 'Content-Type': 'application/json', 'X-CSRF-Token': 'a'.repeat(43) },
    }));
  });

  it('does not send a mutation without CSRF', async () => {
    const fetch = respond({ data: {}, request_id: 'request-1' });
    await expect(apiFetch('/api/logout', { method: 'POST' })).rejects.toThrow('Open a new form');
    expect(fetch).not.toHaveBeenCalled();
  });

  it.each([401, 403, 409, 413, 422, 429, 503])('preserves HTTP %s as a business error', async (status) => {
    respond({ error: { code: 'PUBLIC_CODE', message: 'Safe message.', fields: { title: 'Required.' } }, request_id: 'request-1' }, status);
    await expect(apiFetch('/api/databases')).rejects.toMatchObject({
      name: 'ApiError', status, code: 'PUBLIC_CODE', message: 'Safe message.', requestId: 'request-1', fields: { title: 'Required.' },
    });
  });

  it('distinguishes a failed connection from a business error without leaking the cause', async () => {
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('secret-session-token')));
    const error = await apiFetch('/api/session').catch((error: unknown) => error);
    expect(error).toBeInstanceOf(NetworkError);
    expect(error).not.toBeInstanceOf(ApiError);
    expect(String(error)).not.toContain('secret-session-token');
  });

  it.each([200, 404, 405])('rejects an HTML response with HTTP %s', async (status) => {
    respond('<html>private body</html>', status, 'text/html');
    await expect(apiFetch('/api/databases')).rejects.toBeInstanceOf(ProtocolError);
  });

  it('rejects invalid JSON and mixed envelopes', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(new Response('{', { headers: { 'Content-Type': 'application/json' } })));
    await expect(apiFetch('/api/session')).rejects.toBeInstanceOf(ProtocolError);
    respond({ data: {}, error: { code: 'FAILED', message: 'Failed.' }, request_id: 'request-1' });
    await expect(apiFetch('/api/session')).rejects.toBeInstanceOf(ProtocolError);
  });

  it('preserves intentional cancellation', async () => {
    const controller = new AbortController();
    controller.abort();
    const aborted = new DOMException('Aborted', 'AbortError');
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(aborted));
    await expect(apiFetch('/api/session', { signal: controller.signal })).rejects.toBe(aborted);
  });

  it.each(['https://example.test/api/session', '/api/../logout', '/api/%2e%2e/logout'])('rejects a path outside the API: %s', async (path) => {
    const fetch = respond({ data: {}, request_id: 'request-1' });
    await expect(apiFetch(path)).rejects.toThrow('Invalid API path');
    expect(fetch).not.toHaveBeenCalled();
  });
});
