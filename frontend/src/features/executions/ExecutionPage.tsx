import { useRef } from 'react';
import { useMutation, useQuery } from '@tanstack/react-query';
import { Link, Navigate, useLocation, useNavigate, useParams } from 'react-router';
import { Failure, Layout, Pager, type WorkspaceDatabase } from '../../app/ui';
import { apiFetch, ApiError, type Page } from '../../lib/api';
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

// Rasio progres dihitung dengan BigInt agar penghitung besar tidak kehilangan presisi.
function progressPermille(committed: string, total: string | null): number | null {
  if (total === null) return null;
  try {
    const t = BigInt(total);
    if (t <= BigInt(0)) return null;
    const c = BigInt(committed);
    const value = c >= t ? BigInt(1000) : (c * BigInt(1000)) / t;
    return Number(value);
  } catch {
    return null;
  }
}

export type BatchList = {
  batches?: ExecutionBatch[];
  page?: Page;
  loading?: boolean;
  error?: unknown;
};

export function ExecutionDetail({
  execution,
  batches,
  batchList,
  onRecover,
  recoverPending,
  recoverError,
}: {
  execution: Execution;
  batches?: ExecutionBatch[];
  batchList?: BatchList;
  onRecover?: (action: 'resume' | 'compensate') => void;
  recoverPending?: boolean;
  recoverError?: unknown;
}) {
  const state = execution.outcome ?? execution.phase;
  const permille = progressPermille(execution.committed_rows, execution.total_rows);
  const list: BatchList = batchList ?? { batches };
  const base = `/databases/${encodeURIComponent(execution.target_id)}`;
  return <>
    <p className={`status ${state}`} role="status">
      <State state={state} /> {statusText(execution)}
    </p>
    <dl className="figures">
      <div><dt>Committed rows</dt><dd>{execution.committed_rows}</dd></div>
      <div><dt>Committed batches</dt><dd>{execution.committed_batches}</dd></div>
      {execution.total_rows !== null && <div><dt>Total rows</dt><dd>{execution.total_rows}</dd></div>}
    </dl>
    {permille !== null && <progress className="meter" max={1000} value={permille} aria-label="Committed rows out of total rows" />}
    {execution.failure && <>
      <h2>Failure</h2>
      <dl className="meta failure">
        <dt>Code</dt><dd><code>{execution.failure.code}</code></dd>
        {execution.failure.message && <><dt>Message</dt><dd>{execution.failure.message}</dd></>}
        {execution.failure.sqlstate && <><dt>SQLSTATE</dt><dd><code>{execution.failure.sqlstate}</code></dd></>}
      </dl>
    </>}
    {execution.actions.length > 0 && <>
      <h2>Recovery actions</h2>
      <p className="callout" data-kicker="APPROVAL">Recovery creates a new approval request that requires every group leader to vote before execution.</p>
      {recoverError ? <FormError error={recoverError} /> : null}
      <div className="actions">
        {execution.actions.map((item) => {
          const supported = item.action === 'resume' || item.action === 'compensate';
          return <div key={item.action} className="action-item">
            <button
              type="button"
              className={item.action === 'compensate' ? 'quiet' : 'execute'}
              disabled={!item.enabled || !supported || Boolean(recoverPending)}
              onClick={() => { if (supported && onRecover) onRecover(item.action as 'resume' | 'compensate'); }}
            >
              {actionLabel(item.action)}
            </button>
            {!item.enabled && item.reason_code && <span className="muted"><code>{item.reason_code}</code></span>}
          </div>;
        })}
      </div>
    </>}
    <h2>Details</h2>
    <dl className="meta">
      <dt>Database</dt><dd><code>{execution.target_id}</code></dd>
      <dt>Direction</dt><dd>{execution.direction}</dd>
      <dt>Mode</dt><dd>{execution.mode}</dd>
      {execution.has_partial_commits && <><dt>Partial commits</dt><dd>Yes</dd></>}
      {execution.reconciliation_pending && <><dt>Reconciliation</dt><dd>Pending</dd></>}
      <dt>Created</dt><dd>{`${utc(execution.created_at)} UTC`}</dd>
      {execution.started_at && <><dt>Started</dt><dd>{`${utc(execution.started_at)} UTC`}</dd></>}
      {execution.finished_at && <><dt>Finished</dt><dd>{`${utc(execution.finished_at)} UTC`}</dd></>}
      <dt>Execution ID</dt><dd><code>{execution.id}</code></dd>
    </dl>
    {execution.mode === 'batch' && <>
      <h2>Committed batches</h2>
      {list.loading ? <p className="list-state" role="status">Loading committed batches…</p>
        : list.error && !list.batches ? <FormError error={list.error} />
        : list.batches && list.batches.length > 0 ? <>
          {list.error ? <FormError error={list.error} /> : null}
          <div className="table-wrap"><table>
            <caption className="sr-only">Committed batch checkpoints</caption>
            <thead>
              <tr><th>Batch</th><th>Direction</th><th>Rows</th><th>Cursor end</th><th>Committed (UTC)</th></tr>
            </thead>
            <tbody>
              {list.batches.map((b) => (
                <tr key={`${b.direction}-${b.batch_ordinal}`}>
                  <td>{b.batch_ordinal}</td>
                  <td>{b.direction}</td>
                  <td>{b.rows_affected}</td>
                  <td><code>{b.cursor_end ?? '—'}</code></td>
                  <td>{utc(b.committed_at)}</td>
                </tr>
              ))}
            </tbody>
          </table></div>
          <Pager page={list.page} />
        </>
        : <p className="list-state">No batch has committed yet.</p>}
    </>}
    <p className="back">
      <Link to={`${base}/history`}>Committed history</Link>
      {' · '}
      <Link to={`${base}/requests`}>Review requests</Link>
      {' · '}
      <Link to="/databases">Back to databases</Link>
    </p>
  </>;
}

export function ExecutionPage({ session }: { session: Session }) {
  const navigate = useNavigate();
  const { search } = useLocation();
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
  // Halaman batch dibaca dari URL (?limit&offset) agar Next/Previous dan Back/Forward konsisten.
  const params = new URLSearchParams(search);
  const batchSearch = new URLSearchParams({ limit: params.get('limit') ?? '50', offset: params.get('offset') ?? '0' }).toString();
  const batchesQuery = useQuery({
    queryKey: ['execution-batches', session.user?.id, id, executionID, batchSearch, execution?.committed_batches],
    queryFn: ({ signal }) => apiFetch<{ database?: WorkspaceDatabase; batches: ExecutionBatch[] }>(`${path}/batches?${batchSearch}`, { signal }),
    enabled: session.state === 'authenticated' && execution?.mode === 'batch',
    placeholderData: (previous) => previous,
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
  return <Layout session={session} database={batchesQuery.data?.data.database} title="Execution">
    {query.error && execution && <FormError error={query.error} />}
    {query.isPending ? <p className="page-intro" role="status">Loading…</p> :
      query.isError && !execution ? <Failure error={query.error} /> :
      execution ? (
        <ExecutionDetail
          execution={execution}
          batchList={{
            batches: batchesQuery.data?.data.batches,
            page: batchesQuery.data?.page,
            loading: batchesQuery.isPending,
            error: batchesQuery.error,
          }}
          onRecover={(action) => { if (!recover.isPending) recover.mutate(action); }}
          recoverPending={recover.isPending}
          recoverError={recover.error}
        />
      ) : null}
    {!execution && <p className="back"><Link to="/databases">Back to databases</Link></p>}
  </Layout>;
}
