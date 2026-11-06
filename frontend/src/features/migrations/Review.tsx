import { useEffect } from 'react';
import { useMutation } from '@tanstack/react-query';
import { Link, Navigate, useNavigate } from 'react-router';
import { apiFetch, ApiError } from '../../lib/api';
import type { Session } from '../../lib/session';
import { Layout, type WorkspaceDatabase } from '../../app/ui';
import { FormError, SQL, type Draft, type Receipt } from './shared';

export type ReviewData = { database: WorkspaceDatabase; draft: Draft | null; expected_revision: string; request_key: string; receipt: Receipt | null };

export function Review({ session, base, draftID, review, search, readError }: {
  session: Session; base: string; draftID: string; review?: ReviewData; search: string; readError?: unknown;
}) {
  const navigate = useNavigate();
  const params = new URLSearchParams(search);
  const revision = review?.expected_revision ?? params.get('expected_revision') ?? '';
  const key = review?.request_key ?? params.get('request_key') ?? '';
  const canonical = `${base}/drafts/${draftID}/review?${new URLSearchParams({ expected_revision: revision, request_key: key })}`;
  useEffect(() => {
    if (review && !search) navigate(canonical, { replace: true });
  }, [review, search, canonical, navigate]);
  const execute = useMutation({
    mutationFn: () => apiFetch<Receipt>(`/api${base}/drafts/${draftID}/execute`, { method: 'POST', csrfToken: session.csrf_token,
      body: { expected_revision: revision, request_key: key } }),
    onSuccess: (result) => navigate(`${base}/migrations/${result.data.id}`, { replace: true }),
  });
  if (review?.receipt) return <Navigate to={`${base}/migrations/${review.receipt.id}`} replace />;
  const error = execute.error ?? readError;
  const draft = error ? undefined : review?.draft;
  const requestId = error instanceof ApiError ? error.requestId : '';
  return <Layout session={session} database={review?.database} title={error ? 'Check migration request' : 'Review migration'}>
    {error ? <>
      <FormError error={error} />
      <p>Error code: <code>{error instanceof ApiError ? error.code : 'NETWORK_ERROR'}</code>{requestId && <>{' · Request ID: '}<code>{requestId}</code></>}</p>
      <p>The result may already be recorded. Check this request before deciding what to do next.</p>
      <p><Link to={`${base}/migrations?${new URLSearchParams({ request_key: key })}`}>Find this request's receipt</Link>{' · '}<Link to={canonical}>Open the same review</Link></p>
    </> : null}
    <dl className="meta">
      <dt>Database</dt><dd>{review?.database.name}</dd>
      <dt>Draft</dt><dd>{draft?.title}</dd>
      <dt>Revision</dt><dd>{revision}</dd>
      <dt>Request key</dt><dd><code>{key}</code></dd>
    </dl>
    {!error && draft && <>
      <h2>SQL to run</h2>
      <SQL sql={draft.sql ?? ''} />
      <p className="callout">All statements run in one transaction. If any statement fails, none are applied.</p>
      <form method="post" action={`${base}/drafts/${draftID}/execute`} onSubmit={(event) => { event.preventDefault(); if (!execute.isPending) execute.mutate(); }}>
        <input type="hidden" name="_csrf" value={session.csrf_token} />
        <input type="hidden" name="expected_revision" value={revision} />
        <input type="hidden" name="request_key" value={key} />
        <button type="submit" className="execute" disabled={execute.isPending || !search}>Execute saved revision</button>
      </form>
    </>}
    <p className="back"><Link to={`${base}/drafts/${draftID}`}>Back to the editor</Link></p>
  </Layout>;
}
