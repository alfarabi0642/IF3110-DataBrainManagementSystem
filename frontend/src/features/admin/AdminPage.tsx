import { Fragment, useState, type ReactNode } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Navigate, useLocation, useParams } from 'react-router';
import { apiFetch, ApiError, type Envelope } from '../../lib/api';
import type { Session } from '../../lib/session';
import { Failure, Layout } from '../../app/ui';
import { Users, UserProfile } from './Users';
import { Groups, GroupProfile } from './Groups';
import { Connections, ConnectionProfile } from './Connections';
import { FormError, type User, type UserDetail, type Group, type GroupDetail, type Connection } from './shared';

function Read<T>({ session, title, path, children }: { session: Session; title: string; path: string; children: (value: Envelope<T>, saved: () => void) => ReactNode }) {
  const [version, setVersion] = useState(0);
  const query = useQuery({ queryKey: ['admin', path], queryFn: ({ signal }) => apiFetch<T>(`/api${path}`, { signal }) });
  const denied = !query.data && query.error instanceof ApiError && query.error.code === 'ADMIN_REQUIRED';
  return <Layout session={denied ? undefined : session} title={query.isError && !query.data ? 'Error' : title}>
    {query.isPending ? <p className="page-intro" role="status">Loading…</p> : query.data ? <Fragment key={version}>
      {query.isError && <FormError error={query.error} />}
      {children(query.data, () => setVersion((value) => value + 1))}
    </Fragment> : <Failure error={query.error} />}
  </Layout>;
}

export function AdminPage({ session }: { session: Session }) {
  const { resource, id } = useParams();
  const { pathname, search } = useLocation();
  if (session.state !== 'authenticated') return <Navigate to="/login" replace />;
  const path = pathname + search;
  switch (resource) {
    case 'users': return id ? <Read<UserDetail> session={session} title="User" path={path}>{({ data }, saved) => <UserProfile user={data} session={session} saved={saved} />}</Read> : <Read<User[]> session={session} title="Users" path={path}>{({ data, page }) => <Users users={data} page={page} session={session} />}</Read>;
    case 'groups': return id ? <Read<GroupDetail> session={session} title="Group" path={path}>{({ data }, saved) => <GroupProfile group={data} session={session} saved={saved} />}</Read> : <Read<Group[]> session={session} title="Groups" path={path}>{({ data, page }) => <Groups groups={data} page={page} session={session} />}</Read>;
    case 'databases': return id ? <Read<Connection> session={session} title="Connection" path={path}>{({ data }, saved) => <ConnectionProfile connection={data} session={session} saved={saved} />}</Read> : <Read<Connection[]> session={session} title="Connections" path={path}>{({ data, page }) => <Connections connections={data} page={page} session={session} />}</Read>;
    default: return <Layout title="Error"><Failure error={new ApiError(404, 'ROUTE_NOT_FOUND', 'Page not found.', '')} /></Layout>;
  }
}
