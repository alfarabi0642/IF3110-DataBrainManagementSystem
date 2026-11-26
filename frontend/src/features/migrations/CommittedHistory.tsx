import { useEffect, useRef } from 'react';
import { Link, Navigate, useNavigate, useParams, useSearchParams } from 'react-router';
import { useMutation, useQuery } from '@tanstack/react-query';
import { Failure, Layout, Pager, type WorkspaceDatabase } from '../../app/ui';
import { apiFetch } from '../../lib/api';
import type { Session } from '../../lib/session';
import { FormError, SQL, State, utc, type ActivityItem, type CommittedEvent, type HeadState, type SubmittedRequest } from './shared';

type HistoryData = { database: WorkspaceDatabase; events: CommittedEvent[]; head_state?: HeadState };
type EventData = { database: WorkspaceDatabase; event: CommittedEvent };
type ActivityData = { database: WorkspaceDatabase; activity: ActivityItem[] };

export function HeadControls({
  base,
  headState,
  canEdit,
  onRollback,
  onForward,
  pending,
  error,
}: {
  base: string;
  headState: HeadState;
  canEdit: boolean;
  onRollback?: () => void;
  onForward?: () => void;
  pending?: boolean;
  error?: unknown;
}) {
  return <section aria-label="Current migration head">
    <h2>Current head</h2>
    <dl className="meta">
      <dt>Head generation</dt><dd>{headState.head_generation}</dd>
      <dt>Active migration</dt>
      <dd>{headState.head
        ? <><Link to={`${base}/history/events/${headState.head.execution_id}`}>{headState.head.title}</Link>{` (rev ${headState.head.revision})`}{headState.head.irreversible ? ' · irreversible' : ''}</>
        : 'None (base state)'}</dd>
      <dt>Redo stack</dt>
      <dd>{headState.next_forward
        ? `${headState.redo_depth} pending · next forward: ${headState.next_forward.title} (rev ${headState.next_forward.revision})`
        : 'Empty'}</dd>
    </dl>
    {error ? <FormError error={error} /> : null}
    {canEdit && (headState.head || headState.next_forward) && <>
      <p className="note">Rollback and forward each create a fresh approval request requiring every group leader to vote before execution.</p>
      <div className="actions">
        {headState.head && (
          <button
            type="button"
            className="quiet"
            disabled={!headState.head.can_rollback || Boolean(pending)}
            onClick={() => { if (onRollback) onRollback(); }}
          >
            {headState.head.can_rollback ? `Request rollback: ${headState.head.title}` : 'Active head is irreversible'}
          </button>
        )}
        {headState.next_forward && (
          <button
            type="button"
            className="execute"
            disabled={Boolean(pending)}
            onClick={() => { if (onForward) onForward(); }}
          >
            {`Request forward: ${headState.next_forward.title}`}
          </button>
        )}
      </div>
    </>}
  </section>;
}

export function CommittedEventTable({ base, events }: { base: string; events: CommittedEvent[] }) {
  return <div className="table-wrap"><table>
    <caption className="sr-only">Committed migration events</caption>
    <thead><tr><th>Generation</th><th>Migration</th><th>Direction</th><th>Actor</th><th>Revision</th><th>Committed (UTC)</th></tr></thead>
    <tbody>{events.map((event) => <tr key={event.id}>
      <td>{event.generation}</td>
      <td><Link to={`${base}/history/events/${event.execution_id}`}>{event.title}</Link></td>
      <td>{event.direction}</td><td>{event.actor}</td><td>{event.revision}</td>
      <td>{utc(event.committed_at)}</td>
    </tr>)}</tbody>
  </table></div>;
}

export function ActivityTable({ base, activity }: { base: string; activity: ActivityItem[] }) {
  return <div className="table-wrap"><table>
    <caption className="sr-only">All activity including failures</caption>
    <thead><tr><th>Kind</th><th>Migration</th><th>Actor</th><th>State</th><th>Created (UTC)</th></tr></thead>
    <tbody>{activity.map((item) => <tr key={`${item.source_kind}:${item.id}`}>
      <td>{item.source_kind === 'legacy_attempt' ? 'Legacy attempt' : item.source_kind === 'worker_attempt' ? 'Worker attempt' : 'Execution'}</td>
      <td>{item.source_kind === 'legacy_attempt'
        ? <Link to={`${base}/migrations/${item.attempt_id}`}>{item.title}</Link>
        : <Link to={`${base}/executions/${item.execution_id}`}>{item.title}{item.attempt_number ? ` · attempt ${item.attempt_number}` : ''}</Link>}</td>
      <td>{item.actor}</td><td><State state={item.state} /></td><td>{utc(item.created_at)}</td>
    </tr>)}</tbody>
  </table></div>;
}

function EventDetail({ base, event }: { base: string; event: CommittedEvent }) {
  return <>
    <dl className="meta">
      <dt>Status</dt><dd>{event.status}</dd>
      <dt>Direction / mode</dt><dd>{event.direction} / {event.mode}</dd>
      <dt>Actor</dt><dd>{event.actor}</dd>
      <dt>Revision</dt><dd>{event.revision}</dd>
      <dt>Generation</dt><dd>{event.generation}</dd>
      <dt>Committed</dt><dd>{utc(event.committed_at)} UTC</dd>
      {event.parent_execution_id && <><dt>Parent execution</dt><dd><Link to={`${base}/executions/${event.parent_execution_id}`}>{event.parent_execution_id}</Link></dd></>}
      {event.canceled_execution_id && <><dt>Canceled execution</dt><dd><Link to={`${base}/executions/${event.canceled_execution_id}`}>{event.canceled_execution_id}</Link></dd></>}
      <dt>Plan hash</dt><dd><code>{event.plan_hash}</code></dd>
    </dl>
    <h2>Up migration (frozen)</h2><SQL sql={event.sql ?? ''} />
    <h2>Down migration (frozen)</h2>
    {event.irreversible ? <p className="callout"><strong>Irreversible.</strong> {event.irreversible_reason || 'No reason recorded.'}</p> : <SQL sql={event.down_sql ?? ''} />}
    <p className="back"><Link to={`${base}/history?hide_rollback=false`}>All committed history</Link></p>
  </>;
}

export function CommittedHistoryPage({ session }: { session: Session }) {
  const { id = '', executionID } = useParams();
  const base = `/databases/${encodeURIComponent(id)}`;
  const [search, setSearch] = useSearchParams();
  const navigate = useNavigate();
  const hide = search.get('hide_rollback') === 'true';
  const keysRef = useRef<{ rollback: string; forward: string }>({
    rollback: typeof crypto !== 'undefined' && 'randomUUID' in crypto ? crypto.randomUUID() : '00000000-0000-4000-8000-000000000011',
    forward: typeof crypto !== 'undefined' && 'randomUUID' in crypto ? crypto.randomUUID() : '00000000-0000-4000-8000-000000000012',
  });
  useEffect(() => {
    if (!search.has('hide_rollback')) {
      const next = new URLSearchParams(search);
      next.set('hide_rollback', 'false');
      setSearch(next, { replace: true });
    }
  }, [search, setSearch]);
  const historyPath = `${base}/history?${new URLSearchParams({ hide_rollback: String(hide), limit: search.get('limit') ?? '50', offset: search.get('offset') ?? '0' })}`;
  const committed = useQuery({
    queryKey: ['committed-history', session.user?.id, historyPath],
    queryFn: ({ signal }) => apiFetch<HistoryData>(`/api${historyPath}`, { signal }),
    enabled: session.state === 'authenticated' && !executionID, gcTime: 0,
  });
  const event = useQuery({
    queryKey: ['committed-event', session.user?.id, base, executionID],
    queryFn: ({ signal }) => apiFetch<EventData>(`/api${base}/history/events/${encodeURIComponent(executionID ?? '')}`, { signal }),
    enabled: session.state === 'authenticated' && !!executionID, gcTime: 0,
  });
  const activityOffset = search.get('activity_offset') ?? '0';
  const activityPath = `${base}/activity?limit=50&offset=${encodeURIComponent(activityOffset)}`;
  const activity = useQuery({
    queryKey: ['activity', session.user?.id, activityPath],
    queryFn: ({ signal }) => apiFetch<ActivityData>(`/api${activityPath}`, { signal }),
    enabled: session.state === 'authenticated', gcTime: 0,
  });
  const headState = committed.data?.data.head_state;
  const moveHead = useMutation({
    mutationFn: (direction: 'rollback' | 'forward') => {
      if (!headState) throw new Error('Head state is not loaded.');
      if (direction === 'rollback') {
        if (!headState.head) throw new Error('No active head to roll back.');
        return apiFetch<{ request: SubmittedRequest }>(`/api${base}/migrations/${encodeURIComponent(headState.head.definition_id)}/rollback-request`, {
          method: 'POST',
          csrfToken: session.csrf_token,
          body: {
            source_execution_id: headState.head.execution_id,
            expected_revision: headState.head_generation,
            request_key: keysRef.current.rollback,
          },
        });
      }
      if (!headState.next_forward) throw new Error('No forward migration available.');
      return apiFetch<{ request: SubmittedRequest }>(`/api${base}/migrations/${encodeURIComponent(headState.next_forward.definition_id)}/forward-request`, {
        method: 'POST',
        csrfToken: session.csrf_token,
        body: {
          source_execution_id: headState.next_forward.source_execution_id,
          expected_revision: headState.head_generation,
          request_key: keysRef.current.forward,
        },
      });
    },
    onSuccess: (result) => navigate(`${base}/requests/${encodeURIComponent(result.data.request.id)}`),
  });
  if (session.state !== 'authenticated') return <Navigate to="/login" replace />;
  const loading = executionID ? event.isPending : committed.isPending;
  const error = executionID ? event.error : committed.error;
  const database = (executionID ? event.data?.data.database : committed.data?.data.database) ?? activity.data?.data.database;
  if (loading) return <Layout session={session} title="Committed history"><p className="page-intro" role="status">Loading…</p></Layout>;
  if (error && !(executionID ? event.data : committed.data)) return <Layout session={session} title="Committed history"><Failure error={error} /></Layout>;
  if (executionID) {
    const detail = event.data?.data.event;
    return <Layout session={session} database={database} title={`Committed event ${detail?.title ?? ''}`}>
      {detail ? <EventDetail base={base} event={detail} /> : <Failure error={error} />}
    </Layout>;
  }
  const data = committed.data?.data;
  const events = data?.events ?? [];
  const page = committed.data?.page;
  return <Layout session={session} database={database} title="Committed history">
    <p className="page-intro">Append-only committed events from the current head model. Rollback pairs can be hidden without deleting or changing any event; failed activity remains visible below.</p>
    {headState && (
      <HeadControls
        base={base}
        headState={headState}
        canEdit={database?.access === 'view_edit'}
        onRollback={() => { if (!moveHead.isPending) moveHead.mutate('rollback'); }}
        onForward={() => { if (!moveHead.isPending) moveHead.mutate('forward'); }}
        pending={moveHead.isPending}
        error={moveHead.error}
      />
    )}
    <form className="searchbar" onSubmit={(event) => event.preventDefault()} method="get" action={`${base}/history`}>
      <label><input type="checkbox" checked={hide} onChange={(change) => {
        const next = new URLSearchParams(search);
        next.set('hide_rollback', String(change.target.checked));
        next.delete('offset');
        navigate(`${base}/history?${next}`, { replace: true });
      }} /> Hide rollback pairs</label>
    </form>
    {committed.isError && <FormError error={committed.error} />}
    {events.length ? <><CommittedEventTable base={base} events={events} /><Pager page={page} /></> : <p>No committed migrations yet.</p>}
    <h2>Activity (including failed attempts)</h2>
    {activity.isError && <FormError error={activity.error} />}
    {activity.data?.data.activity.length ? <ActivityTable base={base} activity={activity.data.data.activity} /> : <p>No activity recorded yet.</p>}
    {activity.data?.page && (activity.data.page.prev_offset !== null || activity.data.page.next_offset !== null) && <nav className="pager" aria-label="Activity pages">
      {activity.data.page.prev_offset !== null && <button type="button" onClick={() => {
        const next = new URLSearchParams(search);
        next.set('activity_offset', String(activity.data!.page!.prev_offset));
        navigate(`${base}/history?${next}`);
      }}>Newer activity</button>}
      {activity.data.page.next_offset !== null && <button type="button" onClick={() => {
        const next = new URLSearchParams(search);
        next.set('activity_offset', String(activity.data!.page!.next_offset));
        navigate(`${base}/history?${next}`);
      }}>Older activity</button>}
    </nav>}
    <p className="back"><Link to={`${base}/migrations`}>Legacy attempt history</Link></p>
  </Layout>;
}
