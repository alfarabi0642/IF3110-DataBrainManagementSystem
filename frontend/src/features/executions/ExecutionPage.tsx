import { useRef } from 'react';
import { useMutation, useQuery } from '@tanstack/react-query';
import { Link, Navigate, useNavigate, useParams } from 'react-router';
import { Failure, Layout } from '../../app/ui';
import { apiFetch, ApiError } from '../../lib/api';
import type { Session } from '../../lib/session';
import { FormError, State, utc, type SubmittedRequest } from '../migrations/shared';

export type Execution = {
  id: string;
  target_id: string;
  direction: string;
  mode: string;
  phase: 'queued' | 'running' | 'blocked' | 'finished';
  outcome: 'succeeded' | 'failed' | 'partial' | 'partially_reverted' | 'unknown' | null;
  committed_rows: string;
  committed_batches: string;
  total_rows: string | null;
  has_partial_commits: boolean;
  reconciliation_pending: boolean;
  created_at: string;
  started_at: string | null;
  finished_at: string | null;
  failure?: { code: string; message: string; sqlstate: string };
  actions: { action: string; enabled: boolean; reason_code: string }[];
};

export type ExecutionBatch = {
  batch_ordinal: string;
  direction: string;
  action_execution_id: string;
  rows_affected: string;
  cursor_end: string | null;
  committed_at: string;
};

// Polling mengikuti kontrak: 2 detik saat aktif, 10 detik saat blocked/unknown, berhenti saat
// selesai. GET tidak pernah memperbaiki state.
export function executionPollInterval(execution?: Execution): number | false {
  if (!execution || execution.phase === 'finished') return false;
  if (execution.phase === 'blocked' || execution.outcome === 'unknown' || execution.outcome === 'partially_reverted') return 10000;
  return 2000;
}

function statusText(execution: Execution) {
  if (execution.phase === 'finished') {
    if (execution.outcome === 'succeeded') return 'The execution committed successfully.';
    if (execution.outcome === 'failed') return execution.has_partial_commits
      ? 'The execution failed after some batches had committed. The database remains blocked.'
      : 'The execution failed. No changes from this execution were committed.';
    return 'The execution is finished.';
  }
  if (execution.phase === 'blocked') {
    if (execution.outcome === 'unknown') return 'The outcome is not confirmed. New work on this database stays blocked until the evidence is reconciled.';
    if (execution.outcome === 'partial') return 'Some batches committed, but the execution did not finish. The database remains blocked.';
    if (execution.outcome === 'partially_reverted') return 'Compensation stopped after some batches were reverted. The database remains blocked.';
    return 'The execution is blocked.';
  }
  if (execution.phase === 'running') return 'The execution is running. Counts include only committed batches.';
  return 'The execution is queued. A worker has not started it yet.';
}

function actionLabel(action: string) {
  if (action === 'resume') return 'Request approved resume';
  if (action === 'compensate') return 'Request approved reverse compensation';
  return action;
}

export function ExecutionDetail({
  execution,
  batches,
  onRecover,
  recoverPending,
  recoverError,
}: {
  execution: Execution;
  batches?: ExecutionBatch[];
  onRecover?: (action: 'resume' | 'compensate') => void;
  recoverPending?: boolean;
  recoverError?: unknown;
}) {
  const state = execution.outcome ?? execution.phase;
  return <>
    <p className={`status ${state}`} role="status">
      <State state={state} /> {statusText(execution)}
    </p>
    <h2>Details</h2>
    <dl className="meta">
      <dt>Database</dt><dd><code>{execution.target_id}</code></dd>
      <dt>Direction</dt><dd>{execution.direction}</dd>
      <dt>Mode</dt><dd>{execution.mode}</dd>
      <dt>Committed rows</dt><dd>{execution.committed_rows}</dd>
      <dt>Committed batches</dt><dd>{execution.committed_batches}</dd>
      {execution.total_rows !== null && <><dt>Total rows</dt><dd>{execution.total_rows}</dd></>}
      {execution.has_partial_commits && <><dt>Partial commits</dt><dd>Yes</dd></>}
      {execution.reconciliation_pending && <><dt>Reconciliation</dt><dd>Pending</dd></>}
      <dt>Created</dt><dd>{`${utc(execution.created_at)} UTC`}</dd>
      {execution.started_at && <><dt>Started</dt><dd>{`${utc(execution.started_at)} UTC`}</dd></>}
      {execution.finished_at && <><dt>Finished</dt><dd>{`${utc(execution.finished_at)} UTC`}</dd></>}
      {execution.failure && <>
        <dt>Failure</dt><dd><code>{execution.failure.code}</code>{execution.failure.message ? ` ${execution.failure.message}` : ''}</dd>
        {execution.failure.sqlstate && <><dt>SQLSTATE</dt><dd><code>{execution.failure.sqlstate}</code></dd></>}
      </>}
      <dt>Execution ID</dt><dd><code>{execution.id}</code></dd>
    </dl>
    {execution.actions.length > 0 && <>
      <h2>Recovery actions</h2>
      <p className="callout">Recovery creates a new approval request that requires every group leader to vote before execution.</p>
      {recoverError ? <FormError error={recoverError} /> : null}
      <div className="actions">
        {execution.actions.map((item) => {
          const supported = item.action === 'resume' || item.action === 'compensate';
          return <div key={item.action}>
            <button
              type="button"
              className={item.action === 'compensate' ? 'quiet' : 'execute'}
              disabled={!item.enabled || !supported || Boolean(recoverPending)}
              onClick={() => { if (supported && onRecover) onRecover(item.action as 'resume' | 'compensate'); }}
            >
              {actionLabel(item.action)}
            </button>
            {!item.enabled && item.reason_code && <span className="muted">{' '}<code>{item.reason_code}</code></span>}
          </div>;
        })}
      </div>
    </>}
    {execution.mode === 'batch' && batches && batches.length > 0 && <>
      <h2>Committed batches</h2>
      <table className="data">
        <thead>
          <tr><th>Batch</th><th>Direction</th><th>Rows</th><th>Cursor end</th><th>Committed (UTC)</th></tr>
        </thead>
        <tbody>
          {batches.map((b) => (
            <tr key={`${b.direction}-${b.batch_ordinal}`}>
              <td>{b.batch_ordinal}</td>
              <td>{b.direction}</td>
              <td>{b.rows_affected}</td>
              <td><code>{b.cursor_end ?? '—'}</code></td>
              <td>{utc(b.committed_at)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </>}
    <p className="back">
      <Link to={`/databases/${encodeURIComponent(execution.target_id)}/history`}>Committed history</Link>
      {' · '}
      <Link to={`/databases/${encodeURIComponent(execution.target_id)}/requests`}>Review requests</Link>
      {' · '}
      <Link to="/databases">Back to databases</Link>
    </p>
  </>;
}

export function ExecutionPage({ session }: { session: Session }) {
  const navigate = useNavigate();
  const { id = '', executionID = '' } = useParams();
  const path = `/api/databases/${encodeURIComponent(id)}/executions/${encodeURIComponent(executionID)}`;
  const keysRef = useRef<{ resume: string; compensate: string }>({
    resume: typeof crypto !== 'undefined' && 'randomUUID' in crypto ? crypto.randomUUID() : '00000000-0000-4000-8000-000000000001',
    compensate: typeof crypto !== 'undefined' && 'randomUUID' in crypto ? crypto.randomUUID() : '00000000-0000-4000-8000-000000000002',
  });
  const query = useQuery({
    queryKey: ['execution', session.user?.id, id, executionID],
    queryFn: ({ signal }) => apiFetch<Execution>(path, { signal }),
    enabled: session.state === 'authenticated',
    refetchInterval: (query) => {
      const error = query.state.error;
      if (error instanceof ApiError && [401, 403, 404].includes(error.status)) return false;
      return executionPollInterval(query.state.data?.data);
    },
    retry: (failureCount, error) => failureCount < 2 && !(error instanceof ApiError && [401, 403, 404].includes(error.status)),
    gcTime: 0,
  });
  const execution = query.data?.data;
  const batchesQuery = useQuery({
    queryKey: ['execution-batches', session.user?.id, id, executionID, execution?.committed_batches],
    queryFn: ({ signal }) => apiFetch<{ batches: ExecutionBatch[] }>(`${path}/batches?limit=100&offset=0`, { signal }),
    enabled: session.state === 'authenticated' && execution?.mode === 'batch',
    gcTime: 0,
  });
  const recover = useMutation({
    mutationFn: (action: 'resume' | 'compensate') => {
      const suffix = action === 'resume' ? 'resume-requests' : 'compensation-requests';
      return apiFetch<{ request: SubmittedRequest }>(`${path}/${suffix}`, {
        method: 'POST',
        csrfToken: session.csrf_token,
        body: { request_key: keysRef.current[action] },
      });
    },
    onSuccess: (result) => navigate(`/databases/${encodeURIComponent(id)}/requests/${encodeURIComponent(result.data.request.id)}`),
  });
  if (session.state !== 'authenticated') return <Navigate to="/login" replace />;
  return <Layout session={session} title="Execution">
    {query.error && execution && <FormError error={query.error} />}
    {query.isPending ? <p className="page-intro" role="status">Loading…</p> :
      query.isError && !execution ? <Failure error={query.error} /> :
      execution ? (
        <ExecutionDetail
          execution={execution}
          batches={batchesQuery.data?.data.batches}
          onRecover={(action) => { if (!recover.isPending) recover.mutate(action); }}
          recoverPending={recover.isPending}
          recoverError={recover.error}
        />
      ) : null}
    {!execution && <p className="back"><Link to="/databases">Back to databases</Link></p>}
  </Layout>;
}
