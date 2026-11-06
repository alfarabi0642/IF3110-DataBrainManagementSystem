import { useRef } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Navigate, useLocation, useParams } from 'react-router';
import { Failure, Layout, type WorkspaceDatabase } from '../../app/ui';
import { apiFetch, ApiError, type Envelope } from '../../lib/api';
import type { Session } from '../../lib/session';
import { DraftEditor, Drafts } from './Drafts';
import { AttemptResult, History } from './History';
import { Review, type ReviewData } from './Review';
import { FormError, type Attempt, type Draft, type Receipt } from './shared';

type Data = { database: WorkspaceDatabase; drafts: Draft[]; draft: Draft; attempts: Attempt[]; attempt: Attempt } & ReviewData;

function pageValue(raw: string | null, min: number, max: number) {
  return raw === null || /^-?\d+$/.test(raw) && Number.isSafeInteger(Number(raw)) && Number(raw) >= min && Number(raw) <= max;
}

export function MigrationPage({ session }: { session: Session }) {
  const { id = '', draftID, attemptID } = useParams();
  const { pathname, search, key } = useLocation();
  const base = `/databases/${encodeURIComponent(id)}`;
  const review = pathname.endsWith('/review');
  const history = pathname.endsWith('/migrations');
  const params = new URLSearchParams(search);
  const searchKey = history ? params.get('request_key') : null;
  const receiptSearch = history && params.has('request_key');
  const validReceiptQuery = [...params.keys()].every((name) => ['request_key', 'limit', 'offset'].includes(name)) &&
    [...new Set(params.keys())].every((name) => params.getAll(name).length === 1) &&
    pageValue(params.get('limit'), 1, 200) && pageValue(params.get('offset'), 0, 100000);
  const path = receiptSearch ? `${base}/receipts/${encodeURIComponent(searchKey || 'invalid')}` :
    history ? `${base}/history${search}` : attemptID ? `${base}/attempts/${encodeURIComponent(attemptID)}${search}` : `${pathname}${search}`;
  const query = useQuery({
    queryKey: ['migrations', session.user?.id, path, key],
    queryFn: ({ signal }) => {
      if (receiptSearch && !validReceiptQuery) throw new ApiError(400, 'INVALID_REQUEST', 'Invalid form or query.', '');
      return apiFetch<Data & Receipt>(`/api${path}`, { signal });
    },
    enabled: session.state === 'authenticated', gcTime: 0, refetchOnWindowFocus: false, refetchOnReconnect: false,
  });
  const read = useRef<{ identity: string; value?: Envelope<Data & Receipt> }>({ identity: '' });
  const identity = `${session.user?.id}|${path}|${key}`;
  if (read.current.identity !== identity) read.current = { identity };
  if (query.data) read.current.value = query.data;
  const response = query.data ?? read.current.value;
  const notFound = receiptSearch && query.error instanceof ApiError && query.error.code === 'ATTEMPT_NOT_FOUND';
  const reviewError = review && query.isError && !!params.get('expected_revision') && !!params.get('request_key') &&
    !(query.error instanceof ApiError && ['INVALID_REQUEST', 'DATABASE_NOT_FOUND', 'DATABASE_ACCESS_DENIED', 'SESSION_INVALID'].includes(query.error.code));
  // Metadata tambahan hanya dipakai saat receipt kosong atau review gagal; SQL terkini tidak ditampilkan.
  const metadata = useQuery({
    queryKey: ['migrations', session.user?.id, base, 'metadata', draftID],
    queryFn: ({ signal }) => apiFetch<Data>(`/api${base}/${review ? `drafts/${encodeURIComponent(draftID ?? '')}` : 'history?limit=1'}`, { signal }),
    enabled: session.state === 'authenticated' && (notFound || reviewError), gcTime: 0,
  });
  if (session.state !== 'authenticated') return <Navigate to="/login" replace />;
  if (receiptSearch && query.data) return <Navigate to={`${base}/migrations/${query.data.data.id}`} replace />;
  const result = response?.data;
  const database = result?.database ?? metadata.data?.data.database;
  const title = review ? reviewError ? 'Check migration request' : 'Review migration' : history ? 'Migration history' : attemptID ? 'Migration result' : draftID ? 'Edit draft' : 'Drafts';
  const loading = !response && (query.isPending || ((notFound || reviewError) && metadata.isPending));
  const error = (notFound || reviewError) ? metadata.error : response ? null : query.error;
  const beforeScope = error instanceof ApiError && ['INVALID_REQUEST', 'DATABASE_NOT_FOUND', 'DATABASE_ACCESS_DENIED'].includes(error.code);
  if (!loading && !error && review) return <Review key={key} base={base} draftID={draftID!} session={session} review={result ?? (database ? { database, draft: null, expected_revision: params.get('expected_revision')!, request_key: params.get('request_key')!, receipt: null } : undefined)} search={search} readError={reviewError ? query.error : undefined} />;
  return <Layout session={beforeScope ? undefined : session} database={database} title={error ? 'Error' : title}>
    {response && query.error && <FormError error={query.error} />}
    {loading ? <p className="page-intro" role="status">Loading…</p> : error ? <Failure error={error} /> :
      history ? <History key={key} base={base} attempts={result?.attempts ?? []} page={response?.page} searchKey={searchKey ?? ''} /> :
      attemptID && result ? <AttemptResult base={base} attempt={result.attempt} refresh={() => { void query.refetch(); }} /> :
      draftID && result ? <DraftEditor key={key} base={`${base}/drafts`} draft={result.draft} session={session} /> :
      result ? <Drafts key={key} base={`${base}/drafts`} drafts={result.drafts} page={response?.page} session={session} /> : null}
  </Layout>;
}
