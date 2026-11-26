import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Link, Navigate, useNavigate, useParams } from 'react-router';
import { Failure, Layout, Pager, type WorkspaceDatabase } from '../../app/ui';
import { apiFetch, ApiError } from '../../lib/api';
import type { Session } from '../../lib/session';
import { DefinitionPreview, FormError, State, utc, type Draft, type RequestGroup, type SubmittedRequest } from './shared';

type RequestsData = { database: WorkspaceDatabase; requests: SubmittedRequest[] };
type RequestData = { database: WorkspaceDatabase; request: SubmittedRequest };

export function RequestGroups({ groups, onApprove, pending }: {
  groups: RequestGroup[]; onApprove?: (groupID: string) => void; pending?: boolean;
}) {
  return <div className="table-wrap"><table>
    <caption className="sr-only">Required group decisions</caption>
    <thead><tr><th>Group</th><th>Access</th><th>Leader</th><th>Decision</th><th>Action</th></tr></thead>
    <tbody>{groups.map((group) => <tr key={group.group_id}>
      <td>{group.group_name}</td>
      <td>{group.access === 'view_edit' ? 'View and edit' : 'View only'}</td>
      <td>{group.leader_username || group.leader_id}</td>
      <td>{group.decision
        ? <span>{group.decision.actor} · <time dateTime={group.decision.created_at}>{utc(group.decision.created_at)} UTC</time></span>
        : <span className="muted">Waiting for leader</span>}</td>
      <td>{group.eligible && onApprove
        ? <button type="button" disabled={pending} onClick={() => onApprove(group.group_id)}>Approve for {group.group_name}</button>
        : group.decision ? 'Recorded' : '—'}</td>
    </tr>)}</tbody>
  </table></div>;
}

function RequestDetail({ request, onApprove, pending, onExecute, executePending, canExecute, base }: {
  request: SubmittedRequest; onApprove: (groupID: string) => void; pending: boolean;
  onExecute: () => void; executePending: boolean; canExecute: boolean; base: string;
}) {
  const draft: Draft = { id: request.draft_id, title: request.title, sql: request.sql, down_sql: request.down_sql,
    irreversible: request.irreversible, irreversible_reason: request.irreversible_reason, frozen: true,
    revision: request.revision, updated_by: request.actor, updated_at: request.updated_at };
  return <>
    <dl className="meta">
      <dt>State</dt><dd><State state={request.state} /></dd>
      <dt>Actor</dt><dd>{request.actor}</dd>
      <dt>Revision</dt><dd>{request.revision}</dd>
      <dt>Direction / mode</dt><dd>{request.direction} / {request.mode}</dd>
      <dt>Target config revision</dt><dd>{request.config_revision}</dd>
      <dt>Policy generation</dt><dd>{request.policy_generation}</dd>
      <dt>Head generation</dt><dd>{request.head_generation}</dd>
      <dt>Submitted</dt><dd>{utc(request.created_at)} UTC</dd>
      {request.batch && <>
        <dt>Batch target</dt><dd><code>{`${request.batch.schema}.${request.batch.table} (${request.batch.key_column}, ${request.batch.value_column})`}</code></dd>
        <dt>Batch delta / size</dt><dd>{`${request.batch.delta >= 0 ? '+' : ''}${request.batch.delta} · ${request.batch.batch_size} rows/batch`}</dd>
      </>}
    </dl>
    {request.state === 'stale' && <p className="status failed" role="alert">This request is stale{request.stale_reason ? ` (${request.stale_reason})` : ''}. Prior votes are retained for audit; submit a fresh request.</p>}
    {request.state === 'consumed' && <p className="status">This request has been consumed. It remains immutable for audit.</p>}
    {request.state === 'pending_review' && <p className="note">Waiting for all required group leaders to vote. Voting creates no SQL or job.</p>}
    {request.state === 'approved' && <p className="status">All required groups approved. Admission is owned by the execution workflow.</p>}
    {request.state === 'consumed' && request.execution_id && <p><Link to={`${base}/executions/${request.execution_id}`}>Open the queued execution</Link></p>}
    {request.state === 'approved' && canExecute && <form method="post" action={`${base}/requests/${request.id}/execute`} onSubmit={(event) => { event.preventDefault(); if (!executePending) onExecute(); }}>
      <p className="callout">Queueing freezes the on-file definition and creates one execution/job. No target SQL runs until the worker revalidates it.</p>
      <button type="submit" className="execute" disabled={executePending}>Queue approved execution</button>
    </form>}
    <DefinitionPreview draft={draft} />
    <h2>Group decisions</h2>
    <RequestGroups groups={request.groups ?? []} onApprove={onApprove} pending={pending} />
  </>;
}

export function RequestsPage({ session }: { session: Session }) {
  const { id = '', requestID } = useParams();
  const base = `/databases/${encodeURIComponent(id)}`;
  const path = requestID ? `${base}/requests/${encodeURIComponent(requestID)}` : `${base}/requests`;
  const client = useQueryClient();
  const navigate = useNavigate();
  const [executeKey] = useState(() => crypto.randomUUID());
  const query = useQuery({
    queryKey: ['migration-requests', session.user?.id, path],
    queryFn: ({ signal }) => apiFetch<RequestsData & RequestData>(`/api${path}`, { signal }),
    enabled: session.state === 'authenticated', gcTime: 0, refetchOnWindowFocus: false, refetchOnReconnect: false,
  });
  const approve = useMutation({
    mutationFn: (groupID: string) => apiFetch<RequestData>(`/api${base}/requests/${encodeURIComponent(requestID ?? '')}/approve`, {
      method: 'POST', csrfToken: session.csrf_token, body: { group_id: groupID },
    }),
    onSuccess: async () => { await client.invalidateQueries({ queryKey: ['migration-requests', session.user?.id] }); },
  });
  const execute = useMutation({
    mutationFn: () => apiFetch<{ execution_id: string }>(`/api${base}/requests/${encodeURIComponent(requestID ?? '')}/execute`, {
      method: 'POST', csrfToken: session.csrf_token, body: { request_key: executeKey },
    }),
    onSuccess: (result) => navigate(`${base}/executions/${result.data.execution_id}`),
  });
  if (session.state !== 'authenticated') return <Navigate to="/login" replace />;
  const data = query.data?.data;
  const database = data?.database;
  const request = data && 'request' in data ? data.request : undefined;
  if (query.isPending) return <Layout session={session} title="Review requests"><p className="page-intro" role="status">Loading…</p></Layout>;
  if (query.isError && !data) return <Layout session={session} title="Review requests"><Failure error={query.error} /></Layout>;
  if (requestID && request) return <Layout session={session} database={database} title={`Review request ${request.title}`}>
    {query.isError && <FormError error={query.error} />}
    <FormError error={approve.error ?? execute.error} />
    <RequestDetail base={base} request={request} pending={approve.isPending}
      onApprove={(groupID) => approve.mutate(groupID)}
      onExecute={() => execute.mutate()} executePending={execute.isPending}
      canExecute={request.state === 'approved' && request.actor_id === session.user?.id} />
    <p className="back"><Link to={`${base}/requests`}>All review requests</Link>{' · '}<Link to={`${base}/drafts`}>Drafts</Link></p>
  </Layout>;
  const requests = (data as RequestsData | undefined)?.requests ?? [];
  return <Layout session={session} database={database} title="Review requests">
    {query.isError && <FormError error={query.error} />}
    <p className="page-intro">Submitting a draft freezes its exact definition and asks every effective group leader to vote. Unsubmitted drafts remain editor-only.</p>
    {requests.length ? <><div className="table-wrap"><table>
      <caption className="sr-only">Submitted review requests</caption>
      <thead><tr><th>Title</th><th>Actor</th><th>Revision</th><th>State</th><th>Submitted (UTC)</th></tr></thead>
      <tbody>{requests.map((item) => <tr key={item.id}>
        <td><Link to={`${base}/requests/${item.id}`}>{item.title}</Link></td>
        <td>{item.actor}</td><td>{item.revision}</td><td><State state={item.state} /></td><td>{utc(item.created_at)}</td>
      </tr>)}</tbody>
    </table></div><Pager page={query.data?.page} /></> : <p>No review requests yet. Open a draft and submit it for approval.</p>}
    <p className="back"><Link to={`${base}/drafts`}>Drafts</Link></p>
  </Layout>;
}
