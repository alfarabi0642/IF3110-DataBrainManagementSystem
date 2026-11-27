import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Link, Navigate, useLocation, useNavigate, useParams } from 'react-router';
import { Failure, Layout, Pager, type WorkspaceDatabase } from '../../app/ui';
import { apiFetch } from '../../lib/api';
import type { Session } from '../../lib/session';
import { DefinitionPreview, FormError, State, utc, type Draft, type RequestGroup, type SubmittedRequest } from './shared';

type RequestsData = { database: WorkspaceDatabase; requests: SubmittedRequest[] };
type RequestData = { database: WorkspaceDatabase; request: SubmittedRequest };

export function RequestGroups({ groups, onApprove, pending }: {
  groups: RequestGroup[]; onApprove?: (groupID: string) => void; pending?: boolean;
}) {
  return <div className="table-wrap approvals"><table>
    <caption className="sr-only">Required group decisions</caption>
    <thead><tr><th>Group</th><th>Access</th><th>Leader</th><th>Decision</th><th>Action</th></tr></thead>
    <tbody>{groups.map((group) => <tr key={group.group_id}>
      <td>{group.group_name}</td>
      <td><span className={`access ${group.access}`}>{group.access === 'view_edit' ? 'View and edit' : 'View only'}</span></td>
      <td>{group.leader_username || group.leader_id}</td>
      <td>{group.decision
        ? <><span className="badge succeeded">Approved</span><span className="when">{group.decision.actor} · <time dateTime={group.decision.created_at}>{utc(group.decision.created_at)} UTC</time></span></>
        : <span className="badge pending_review">Waiting for leader</span>}</td>
      <td>{group.eligible && onApprove
        ? <button type="button" disabled={pending} onClick={() => onApprove(group.group_id)}>Approve for {group.group_name}</button>
        : <span className="muted">{group.decision ? 'Recorded' : '—'}</span>}</td>
    </tr>)}</tbody>
  </table></div>;
}

function ApprovalTally({ groups }: { groups: RequestGroup[] }) {
  if (!groups.length) return null;
  const done = groups.filter((group) => group.decision).length;
  return <ul className="tally" aria-label="Approval progress">
    <li className="tally-count" aria-hidden="true">{`${done} of ${groups.length} approved`}</li>
    {groups.map((group) => <li key={group.group_id} className={group.decision ? 'done' : undefined}>
      {group.group_name}<span className="sr-only">{group.decision ? ' approved' : ' waiting'}</span>
    </li>)}
  </ul>;
}

function RequestDetail({ request, onApprove, pending, onExecute, executePending, canExecute, base }: {
  request: SubmittedRequest; onApprove: (groupID: string) => void; pending: boolean;
  onExecute: () => void; executePending: boolean; canExecute: boolean; base: string;
}) {
  const draft: Draft = { id: request.draft_id, title: request.title, sql: request.sql, down_sql: request.down_sql,
    irreversible: request.irreversible, irreversible_reason: request.irreversible_reason, frozen: true,
    revision: request.revision, updated_by: request.actor, updated_at: request.updated_at };
  const groups = request.groups ?? [];
  return <>
    <ApprovalTally groups={groups} />
    {request.state === 'stale' && <p className="status failed" role="alert">This request is stale{request.stale_reason ? ` (${request.stale_reason})` : ''}. Prior votes are retained for audit; submit a fresh request.</p>}
    {request.state === 'consumed' && <p className="status">This request has been consumed. It remains immutable for audit.{request.execution_id && <>{' '}<Link to={`${base}/executions/${request.execution_id}`}>Open the queued execution</Link></>}</p>}
    {request.state === 'pending_review' && <p className="status pending_review">Waiting for all required group leaders to vote. Voting creates no SQL or job.</p>}
    {request.state === 'approved' && <p className="status approved">All required groups approved. Admission is owned by the execution workflow.</p>}
    {request.state === 'approved' && canExecute && <form method="post" action={`${base}/requests/${request.id}/execute`} onSubmit={(event) => { event.preventDefault(); if (!executePending) onExecute(); }}>
      <p className="callout" data-kicker="QUEUE">Queueing freezes the on-file definition and creates one execution/job. No target SQL runs until the worker revalidates it.</p>
      <button type="submit" className="execute" disabled={executePending}>Queue approved execution</button>
    </form>}
    <h2>Request</h2>
    <dl className="meta">
      <dt>State</dt><dd><State state={request.state} /></dd>
      <dt>Actor</dt><dd>{request.actor}</dd>
      <dt>Revision</dt><dd>{request.revision}</dd>
      <dt>Direction / mode</dt><dd>{request.direction} / {request.mode}</dd>
      <dt>Target config revision</dt><dd>{request.config_revision}</dd>
      <dt>Policy generation</dt><dd>{request.policy_generation}</dd>
      <dt>Head generation</dt><dd>{request.head_generation}</dd>
      <dt>Submitted</dt><dd>{utc(request.created_at)} UTC</dd>
    </dl>
    {request.batch && <>
      <h2>Batch parameters</h2>
      <dl className="meta">
        <dt>Target</dt><dd><code>{`${request.batch.schema}.${request.batch.table}`}</code></dd>
        <dt>Key / value</dt><dd><code>{request.batch.key_column}</code> · <code>{request.batch.value_column}</code></dd>
        <dt>Delta</dt><dd><code>{`${request.batch.delta >= 0 ? '+' : ''}${request.batch.delta}`}</code></dd>
        <dt>Batch size</dt><dd>{`${request.batch.batch_size} rows/batch`}</dd>
      </dl>
    </>}
    <DefinitionPreview draft={draft} />
    <h2>Group decisions</h2>
    <RequestGroups groups={groups} onApprove={onApprove} pending={pending} />
  </>;
}

export function RequestsPage({ session }: { session: Session }) {
  const { id = '', requestID } = useParams();
  const { search } = useLocation();
  const base = `/databases/${encodeURIComponent(id)}`;
  // Endpoint daftar hanya menerima limit dan offset; parameter lain di URL tidak diteruskan.
  const listSearch = (() => {
    const from = new URLSearchParams(search);
    const params = new URLSearchParams();
    for (const key of ['limit', 'offset']) { const value = from.get(key); if (value) params.set(key, value); }
    const text = params.toString();
    return text ? `?${text}` : '';
  })();
  const path = requestID ? `${base}/requests/${encodeURIComponent(requestID)}` : `${base}/requests${listSearch}`;
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
  const canEdit = database?.access === 'view_edit';
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
    <p className="back"><Link to={`${base}/requests`}>All review requests</Link>{canEdit && <>{' · '}<Link to={`${base}/drafts`}>Drafts</Link></>}</p>
  </Layout>;
  const requests = (data as RequestsData | undefined)?.requests ?? [];
  return <Layout session={session} database={database} title="Review requests">
    {query.isError && <FormError error={query.error} />}
    <p className="page-intro">Submitting a draft freezes its exact definition and asks every effective group leader to vote. Unsubmitted drafts remain editor-only.</p>
    {requests.length ? <><div className="table-wrap"><table>
      <caption className="sr-only">Submitted review requests</caption>
      <thead><tr><th>Title</th><th>Actor</th><th>Revision</th><th>Mode</th><th>State</th><th>Submitted (UTC)</th></tr></thead>
      <tbody>{requests.map((item) => <tr key={item.id}>
        <td><Link to={`${base}/requests/${item.id}`}>{item.title}</Link></td>
        <td>{item.actor}</td><td>{item.revision}</td><td className="muted">{item.direction} · {item.mode}</td>
        <td><State state={item.state} /></td><td>{utc(item.created_at)}</td>
      </tr>)}</tbody>
    </table></div><Pager page={query.data?.page} /></>
      : <p className="list-state">{Number(new URLSearchParams(search).get('offset') ?? 0) > 0 ? 'No review requests on this page.' : canEdit ? 'No review requests yet. Open a draft and submit it for approval.' : 'No review requests yet.'}</p>}
    {canEdit && <p className="back"><Link to={`${base}/drafts`}>Drafts</Link></p>}
  </Layout>;
}
