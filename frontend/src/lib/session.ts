import { MutationCache, QueryCache, QueryClient, queryOptions } from '@tanstack/react-query';
import { apiFetch, ApiError, type Envelope } from './api';

export type Session = {
  state: 'anonymous' | 'authenticated' | 'reauth_required';
  user: { id: string; username: string; display_name: string; is_platform_admin: boolean } | null;
  csrf_token: string;
  absolute_expires_at: string;
};

export async function clearProtectedCache(client: QueryClient) {
  const protectedQueries = { predicate: (query: { queryKey: readonly unknown[] }) => query.queryKey[0] !== 'session' };
  await client.cancelQueries(protectedQueries);
  client.removeQueries(protectedQueries);
  client.getMutationCache().clear();
}

export function sessionQuery(client: QueryClient) {
  return queryOptions({
    queryKey: ['session'],
    queryFn: async ({ signal }) => {
      const result = await apiFetch<Session>('/api/session', { signal });
      const previous = client.getQueryData<Envelope<Session>>(['session'])?.data;
      const current = result.data;
      // CSRF menandai pergantian family meskipun user tetap sama.
      if (current.state !== 'authenticated' || previous?.csrf_token !== current.csrf_token ||
          JSON.stringify(previous.user) !== JSON.stringify(current.user)) {
        await clearProtectedCache(client);
      }
      return result;
    },
  });
}

export async function refreshSession(client: QueryClient) {
  await client.cancelQueries({ queryKey: ['session'] });
  return client.fetchQuery(sessionQuery(client));
}

export function createQueryClient() {
  const recover = async (error: unknown) => {
    if (error instanceof ApiError && ((error.status === 401 && error.code !== 'INVALID_CREDENTIALS') || error.code === 'CSRF_INVALID')) {
      await clearProtectedCache(client);
      // Bootstrap diperbarui tanpa mengulang mutasi yang ditolak.
      await refreshSession(client).catch(() => {});
    }
  };
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
    queryCache: new QueryCache({
      onError: (error, query) => { if (query.queryKey[0] !== 'session') void recover(error); },
    }),
    mutationCache: new MutationCache({
      onMutate: async (_variables, mutation) => {
        if (mutation.meta?.auth) await client.cancelQueries({ queryKey: ['session'] });
      },
      onError: async (error, _variables, _context, mutation) => {
        if (mutation.meta?.auth && !(error instanceof ApiError)) {
          // Respons auth yang putus dapat terjadi setelah cookie atau revoke berubah.
          await clearProtectedCache(client);
          await refreshSession(client).catch(() => {});
        } else {
          await recover(error);
        }
      },
      onSuccess: async (_data, _variables, _context, mutation) => {
        if (mutation.meta?.auth) await clearProtectedCache(client);
        await refreshSession(client).catch(() => {});
      },
    }),
  });
  return client;
}
