import { useEffect, useState } from 'react';
import { useMutation } from '@tanstack/react-query';
import { Link, Navigate, useNavigate } from 'react-router';
import { apiFetch, ApiError } from '../../lib/api';
import type { Session } from '../../lib/session';
import type { SubmittedRequest } from './shared';
import { Layout, type WorkspaceDatabase } from '../../app/ui';
import { DefinitionPreview, FormError, parseBatchTemplateSQL, type Draft, type Receipt } from './shared';

export type ReviewData = { database: WorkspaceDatabase; draft: Draft | null; expected_revision: string; request_key: string; receipt: Receipt | null };

export function Review({ session, base, draftID, review, search, readError }: {
  session: Session; base: string; draftID: string; review?: ReviewData; search: string; readError?: unknown;
}) {
  const navigate = useNavigate();
  const params = new URLSearchParams(search);
  const revision = review?.expected_revision ?? params.get('expected_revision') ?? '';
  const key = review?.request_key ?? params.get('request_key') ?? '';
  const canonical = `${base}/drafts/${draftID}/review?${new URLSearchParams({ expected_revision: revision, request_key: key })}`;
  const detected = parseBatchTemplateSQL(review?.draft?.sql);
  const [batchEnabled, setBatchEnabled] = useState(Boolean(detected));
  const [schema, setSchema] = useState(detected?.schema ?? 'public');
  const [table, setTable] = useState(detected?.table ?? '');
  const [keyColumn, setKeyColumn] = useState(detected?.keyColumn ?? 'id');
  const [valueColumn, setValueColumn] = useState(detected?.valueColumn ?? '');
  const [delta, setDelta] = useState('1');
  const [batchSize, setBatchSize] = useState('500');
  useEffect(() => {
    if (review && !search) navigate(canonical, { replace: true });
  }, [review, search, canonical, navigate]);
  const submit = useMutation({
    mutationFn: () => {
      const body: Record<string, unknown> = { expected_revision: revision, request_key: key };
      if (batchEnabled) {
        body.batch = {
          schema: schema.trim(),
          table: table.trim(),
          key_column: keyColumn.trim(),
          value_column: valueColumn.trim(),
          delta: Number.parseInt(delta, 10),
          batch_size: Number.parseInt(batchSize, 10),
        };
      }
      return apiFetch<{ request: SubmittedRequest }>(`/api${base}/drafts/${draftID}/submit`, {
        method: 'POST', csrfToken: session.csrf_token, body,
      });
    },
    onSuccess: (result) => navigate(`${base}/requests/${result.data.request.id}`),
  });
  if (review?.receipt) return <Navigate to={`${base}/migrations/${review.receipt.id}`} replace />;
  const error = submit.error ?? readError;
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
      <DefinitionPreview draft={draft} />
      <h2>Submit for review</h2>
      <p className="callout" data-kicker="APPROVAL">Submission freezes this definition and creates one durable request. Every required group leader must vote before admission; approved requests are executed asynchronously by the worker.</p>
      <form method="post" action={`${base}/drafts/${draftID}/submit`} onSubmit={(event) => { event.preventDefault(); if (!submit.isPending) submit.mutate(); }}>
        <input type="hidden" name="_csrf" value={session.csrf_token} />
        <input type="hidden" name="expected_revision" value={revision} />
        <input type="hidden" name="request_key" value={key} />
        <label className="check">
          <input type="checkbox" checked={batchEnabled} onChange={(event) => setBatchEnabled(event.target.checked)} />
          {' '}Use bounded batch execution (v1 numeric-delta recipe)
        </label>
        {batchEnabled && <fieldset className="config">
          <legend>Batch recipe parameters</legend>
          <div className="field-grid">
            <div><label htmlFor="batch-schema">Schema</label>
            <input id="batch-schema" value={schema} onChange={(e) => setSchema(e.target.value)} required /></div>
            <div><label htmlFor="batch-table">Table</label>
            <input id="batch-table" value={table} onChange={(e) => setTable(e.target.value)} required /></div>
            <div><label htmlFor="batch-key">Primary key column (bigint/int)</label>
            <input id="batch-key" value={keyColumn} onChange={(e) => setKeyColumn(e.target.value)} required /></div>
            <div><label htmlFor="batch-value">Numeric value column</label>
            <input id="batch-value" value={valueColumn} onChange={(e) => setValueColumn(e.target.value)} required /></div>
            <div><label htmlFor="batch-delta">Integer delta (non-zero)</label>
            <input id="batch-delta" type="number" value={delta} onChange={(e) => setDelta(e.target.value)} required /></div>
            <div><label htmlFor="batch-size">Batch size (1–10000)</label>
            <input id="batch-size" type="number" min={1} max={10000} value={batchSize} onChange={(e) => setBatchSize(e.target.value)} required /></div>
          </div>
        </fieldset>}
        <p className="submit-preview" aria-live="polite">
          {`Submitting revision ${revision} · `}<b>{batchEnabled ? 'batch' : 'normal'}</b>
          {batchEnabled && <>{' · '}<b>{`${schema || '…'}.${table || '…'}`}</b>{` (${keyColumn || '…'}, ${valueColumn || '…'}) · delta `}<b>{delta || '…'}</b>{` · `}<b>{batchSize || '…'}</b>{' rows/batch'}</>}
        </p>
        <button type="submit" className="execute" disabled={submit.isPending || !search}>
          {batchEnabled ? 'Submit batch request for group approval' : 'Submit for group approval'}
        </button>
      </form>
    </>}
    <p className="back"><Link to={`${base}/drafts/${draftID}`}>Back to the editor</Link></p>
  </Layout>;
}
