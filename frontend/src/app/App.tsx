import { useEffect, useRef, type ReactNode } from 'react';
import { useQuery, useQueryClient } from '@tanstack/react-query';
import { Link, Navigate, Route, Routes, useLocation } from 'react-router';
import { apiFetch, ApiError, type Page } from '../lib/api';

type Session = {
  state: 'anonymous' | 'authenticated' | 'reauth_required';
  user: { id: string; username: string; display_name: string; is_platform_admin: boolean } | null;
  csrf_token: string;
  absolute_expires_at: string;
};

type Database = {
  id: string;
  name: string;
  access: 'view_only' | 'view_edit';
  active_attempt: { id: string; version: string; state: string } | null;
};

function Layout({ session, title, children }: { session?: Session; title: string; children: ReactNode }) {
  const location = useLocation();
  const main = useRef<HTMLElement>(null);
  const previous = useRef(location.key);
  const user = session?.state === 'authenticated' ? session.user : null;
  useEffect(() => {
    document.body.className = user ? 'app' : 'guest';
    document.title = `${title} - DataBrain`;
  }, [user, title]);
  useEffect(() => {
    if (previous.current !== location.key) {
      main.current?.focus({ preventScroll: true });
      window.scrollTo(0, 0);
      previous.current = location.key;
    }
  }, [location.key]);
  return <>
    <a className="skip" href="#main">Skip to content</a>
    <div className="shell">
      <header className="topbar">
        <Link className="brand" to="/"><span className="brand-mark" aria-hidden="true" /><span className="brand-word">Data<em>Brain</em></span></Link>
        {user && <nav className="mainnav" aria-label="Main">
          <span className="nav-label">Workspace</span>
          <Link to="/databases" aria-current={location.pathname === '/databases' ? 'page' : undefined}>Databases</Link>
          {user.is_platform_admin && <>
            <span className="nav-label">Administration</span>
            <a href="/admin/users">Users</a>
            <a href="/admin/groups">Groups</a>
            <a href="/admin/databases">Connections</a>
          </>}
        </nav>}
        {user && <div className="account">
          <span className="avatar" aria-hidden="true">{user.display_name || user.username}</span>
          <span className="who">{user.display_name} <span className="muted">{user.username}</span></span>
          <form method="post" action="/logout">
            <input type="hidden" name="_csrf" value={session?.csrf_token ?? ''} />
            <button type="submit" className="quiet">Log out</button>
          </form>
        </div>}
      </header>
      <main id="main" ref={main} tabIndex={-1}>
        <header className="masthead">
          <p className="eyebrow"><span>{user ? 'Workspace' : 'DataBrain'}</span></p>
          <h1>{title}</h1>
        </header>
        {children}
      </main>
    </div>
  </>;
}

function Failure({ error }: { error: unknown }) {
  return <>
    <p className="status failed" role="alert">{error instanceof Error ? error.message : 'Internal server error.'}</p>
    {error instanceof ApiError && <dl className="meta">
      <dt>Error code</dt><dd><code>{error.code}</code></dd>
      {error.requestId && <><dt>Request ID</dt><dd><code>{error.requestId}</code></dd></>}
    </dl>}
    <p className="back"><Link to="/">Go to the start page</Link></p>
  </>;
}

function DocumentRoute({ to }: { to: string }) {
  useEffect(() => { window.location.replace(to); }, [to]);
  return <p className="page-intro" role="status">Loading…</p>;
}

function Pager({ page }: { page?: Page }) {
  if (!page || (page.prev_offset === null && page.next_offset === null)) return null;
  const link = (offset: number) => `/databases?limit=${page.limit}&offset=${offset}`;
  return <nav className="pager" aria-label="Pages">
    {page.prev_offset !== null && <Link to={link(page.prev_offset)} rel="prev">Previous</Link>}
    {page.next_offset !== null && <Link to={link(page.next_offset)} rel="next">Next</Link>}
  </nav>;
}

function Databases({ session }: { session: Session }) {
  const { search } = useLocation();
  const client = useQueryClient();
  const query = useQuery({
    queryKey: ['databases', session.user?.id, search],
    queryFn: ({ signal }) => apiFetch<Database[]>(`/api/databases${search}`, { signal }),
    enabled: session.state === 'authenticated',
  });
  const expired = query.error instanceof ApiError && query.error.status === 401;
  useEffect(() => {
    if (expired) client.clear();
  }, [expired, client]);
  if (session.state !== 'authenticated' || expired) return <DocumentRoute to="/login" />;
  let content: ReactNode;
  if (query.isPending) content = <p className="page-intro" role="status">Loading…</p>;
  else if (query.isError) content = <Failure error={query.error} />;
  else if (!query.data.data.length) content = <p className="page-intro">You cannot open any database yet. Ask an admin to add your group to one.</p>;
  else content = <>
    <ul className="db-grid" role="list">
      {query.data.data.map((database) => {
        const base = `/databases/${database.id}`;
        const attempt = database.active_attempt;
        return <li className="db-card" key={database.id}>
          <div className="db-card-head">
            <span className="db-glyph" aria-hidden="true" />
            <span className={`access ${database.access}`}>{database.access === 'view_edit' ? 'View and edit' : 'View only'}</span>
          </div>
          <h2 className="db-name">{database.name}</h2>
          <p className="db-progress"><span className="k">In progress</span>{attempt ? <><a href={`${base}/migrations/${attempt.id}`}>{`Version ${attempt.version}`}</a>{' '}<span className={`badge ${attempt.state}`}>{attempt.state}</span></> : 'None'}</p>
          <div className="db-links">
            <a href={`${base}/explorer`} aria-label={`Explorer for ${database.name}`}>Explorer</a>
            {database.access === 'view_edit' && <a href={`${base}/drafts`} aria-label={`Drafts for ${database.name}`}>Drafts</a>}
            <a href={`${base}/migrations`} aria-label={`History for ${database.name}`}>History</a>
          </div>
        </li>;
      })}
    </ul>
    <Pager page={query.data.page} />
  </>;
  return <Layout session={session} title={query.isError ? 'Error' : 'Databases'}>{content}</Layout>;
}

export function App() {
  const location = useLocation();
  const session = useQuery({ queryKey: ['session'], queryFn: ({ signal }) => apiFetch<Session>('/api/session', { signal }) });
  const client = useQueryClient();
  const identity = session.data?.data.user?.id;
  const priorIdentity = useRef(identity);
  useEffect(() => {
    if (priorIdentity.current !== identity || session.data?.data.state !== 'authenticated') {
      client.removeQueries({ queryKey: ['databases'], predicate: (query) => query.queryKey[1] !== identity || session.data?.data.state !== 'authenticated' });
    }
    priorIdentity.current = identity;
  }, [identity, session.data?.data.state, client]);
  const isDatabases = location.pathname === '/' || location.pathname === '/databases';
  if (session.isPending) return <Layout title={isDatabases ? 'Databases' : 'Error'}><p className="page-intro" role="status">Loading…</p></Layout>;
  if (session.isError) return <Layout title="Error"><Failure error={session.error} /></Layout>;
  return <Routes>
      <Route path="/" element={<Navigate to="/databases" replace />} />
      <Route path="/databases" element={<Databases session={session.data.data} />} />
      <Route path="*" element={<Layout title="Error"><Failure error={new ApiError(404, 'ROUTE_NOT_FOUND', 'Page not found.', '')} /></Layout>} />
    </Routes>;
}
