import { useEffect, useRef, type ReactNode } from 'react';
import { useMutation } from '@tanstack/react-query';
import { Link, useLocation } from 'react-router';
import { apiFetch, ApiError, type Page } from '../lib/api';
import type { Session } from '../lib/session';

export type WorkspaceDatabase = { id: string; name: string; access: 'view_only' | 'view_edit' };

export function Layout({ session, title, database, children }: { session?: Session; title: string; database?: WorkspaceDatabase; children: ReactNode }) {
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
            <Link to="/admin/users" aria-current={location.pathname.startsWith('/admin/users') ? 'page' : undefined}>Users</Link>
            <Link to="/admin/groups" aria-current={location.pathname.startsWith('/admin/groups') ? 'page' : undefined}>Groups</Link>
            <Link to="/admin/databases" aria-current={location.pathname.startsWith('/admin/databases') ? 'page' : undefined}>Connections</Link>
          </>}
        </nav>}
        {user && database && <nav className="dbnav" aria-label="Database">
          <span className="nav-label">Database</span>
          <strong>{database.name}</strong>
          <Link to={`/databases/${database.id}/explorer`} aria-current={location.pathname.endsWith('/explorer') ? 'page' : undefined}>Explorer</Link>
          {database.access === 'view_edit' && <a href={`/databases/${database.id}/drafts`}>Drafts</a>}
          <a href={`/databases/${database.id}/migrations`}>History</a>
        </nav>}
        {user && <div className="account">
          <span className="avatar" aria-hidden="true">{user.display_name || user.username}</span>
          <span className="who">{user.display_name} <span className="muted">{user.username}</span></span>
          <LogoutForm session={session!} />
        </div>}
      </header>
      <main id="main" ref={main} tabIndex={-1}>
        <header className="masthead">
          <p className="eyebrow">{database ? <><span>Database</span><span>{database.name}</span></> : <span>{location.pathname.startsWith('/admin/') ? 'Administration' : user ? 'Workspace' : 'DataBrain'}</span>}</p>
          <h1>{title}</h1>
        </header>
        {children}
      </main>
    </div>
  </>;
}

export function Failure({ error }: { error: unknown }) {
  return <>
    <p className="status failed" role="alert">{error instanceof Error ? error.message : 'Internal server error.'}</p>
    {error instanceof ApiError && <dl className="meta">
      <dt>Error code</dt><dd><code>{error.code}</code></dd>
      {error.requestId && <><dt>Request ID</dt><dd><code>{error.requestId}</code></dd></>}
    </dl>}
    <p className="back"><Link to="/">Go to the start page</Link></p>
  </>;
}

export function LogoutForm({ session, reauth = false }: { session: Session; reauth?: boolean }) {
  const logout = useMutation({
    meta: { auth: true },
    mutationFn: () => apiFetch('/api/logout', { method: 'POST', csrfToken: session.csrf_token }),
  });
  return <>
    {logout.error && <p className="status failed" role="alert">{logout.error.message}</p>}
    <form method="post" action="/logout" onSubmit={(event) => { event.preventDefault(); if (!logout.isPending) logout.mutate(); }}>
      <input type="hidden" name="_csrf" value={session.csrf_token} />
      <button type="submit" className="quiet" disabled={logout.isPending}>{reauth ? 'Log out instead' : 'Log out'}</button>
    </form>
  </>;
}

export function Pager({ page }: { page?: Page }) {
  const { pathname, search } = useLocation();
  if (!page || (page.prev_offset === null && page.next_offset === null)) return null;
  const link = (offset: number) => {
    const params = new URLSearchParams(search);
    params.set('limit', String(page.limit));
    params.set('offset', String(offset));
    return `${pathname}?${params}`;
  };
  return <nav className="pager" aria-label="Pages">
    {page.prev_offset !== null && <Link to={link(page.prev_offset)} rel="prev">Previous</Link>}
    {page.next_offset !== null && <Link to={link(page.next_offset)} rel="next">Next</Link>}
  </nav>;
}

