import { useEffect, useRef, useState, type FormEvent, type ReactNode } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Link, Navigate, Route, Routes, useLocation } from 'react-router';
import { apiFetch, ApiError } from '../lib/api';
import { refreshSession, sessionQuery, type Session } from '../lib/session';

import { Layout, Failure, LogoutForm, Pager } from './ui';
import { AdminPage } from '../features/admin/AdminPage';

type Database = {
  id: string;
  name: string;
  access: 'view_only' | 'view_edit';
  active_attempt: { id: string; version: string; state: string } | null;
};

function Login({ session }: { session: Session }) {
  const [username, setUsername] = useState('');
  const password = useRef<HTMLInputElement>(null);
  const usernameInput = useRef<HTMLInputElement>(null);
  const login = useMutation({
    meta: { auth: true },
    mutationFn: () => {
      const normalized = (usernameInput.current?.value ?? '').trim().toLowerCase();
      const value = password.current?.value ?? '';
      if (password.current) password.current.value = '';
      setUsername(normalized);
      // Password tidak menjadi variables yang disimpan MutationCache.
      return apiFetch('/api/login', {
        method: 'POST', csrfToken: session.csrf_token, body: { username: normalized, password: value },
      });
    },
    onSettled: () => { if (password.current) password.current.value = ''; },
    onError: () => password.current?.focus(),
  });
  useEffect(() => { usernameInput.current?.focus(); }, []);
  if (session.state === 'authenticated') return <Navigate to="/databases" replace />;
  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (login.isPending) return;
    login.mutate();
  };
  return <Layout title="Log in">
    <section className="stage" aria-label="About DataBrain">
      <div className="stage-art" aria-hidden="true" />
      <p className="stage-kicker">PostgreSQL workspace</p>
      <p className="stage-line">Explore every table. <em>Migrate</em> with a memory.</p>
      <ul className="stage-facts">
        <li>One transaction per run</li><li>Durable, searchable history</li><li>Group-based access</li>
      </ul>
    </section>
    <div className="auth-panel panel">
      {session.state === 'reauth_required' && <>
        <p className="status unknown" role="status">Your access changed. Please sign in again to continue safely.</p>
        <LogoutForm session={session} reauth />
      </>}
      {login.error && <p className="status failed" role="alert">{login.error.message}</p>}
      <form method="post" action="/login" onSubmit={submit}>
        <input type="hidden" name="_csrf" value={session.csrf_token} />
        <label htmlFor="username">Username</label>
        <input ref={usernameInput} id="username" name="username" autoComplete="username" autoCapitalize="none" spellCheck={false} maxLength={32} required value={username} onChange={(event) => setUsername(event.target.value)} />
        <label htmlFor="password">Password</label>
        <input ref={password} id="password" name="password" type="password" autoComplete="current-password" aria-describedby="password-help" required />
        <p id="password-help" className="form-help">Passwords contain 12–128 characters and are never redisplayed.</p>
        <button type="submit" disabled={login.isPending}>Log in</button>
      </form>
    </div>
  </Layout>;
}

function Databases({ session }: { session: Session }) {
  const { search } = useLocation();
  const query = useQuery({
    queryKey: ['databases', session.user?.id, search],
    queryFn: ({ signal }) => apiFetch<Database[]>(`/api/databases${search}`, { signal }),
    enabled: session.state === 'authenticated',
  });
  const expired = query.error instanceof ApiError && query.error.status === 401;
  if (session.state !== 'authenticated') return <Navigate to="/login" replace />;
  if (expired) return <Layout title="Log in"><p className="page-intro" role="status">Loading…</p></Layout>;
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
  const client = useQueryClient();
  const session = useQuery(sessionQuery(client));
  const deadline = session.data?.data.absolute_expires_at;
  useEffect(() => {
    if (!deadline) return;
    const timer = window.setTimeout(() => { void refreshSession(client).catch(() => {}); }, Math.max(0, Date.parse(deadline) - Date.now()));
    return () => window.clearTimeout(timer);
  }, [deadline, client]);
  const isDatabases = location.pathname === '/' || location.pathname === '/databases';
  if (session.isPending) return <Layout title={location.pathname === '/login' ? 'Log in' : isDatabases ? 'Databases' : 'Error'}><p className="page-intro" role="status">Loading…</p></Layout>;
  if (session.isError) return <Layout title="Error"><Failure error={session.error} /></Layout>;
  return <Routes>
      <Route path="/" element={<Navigate to="/databases" replace />} />
      <Route path="/login" element={<Login key={session.data.data.csrf_token} session={session.data.data} />} />
      <Route path="/databases" element={<Databases session={session.data.data} />} />
      <Route path="/admin/:resource" element={<AdminPage key={location.pathname} session={session.data.data} />} />
      <Route path="/admin/:resource/:id" element={<AdminPage key={location.pathname} session={session.data.data} />} />
      <Route path="*" element={<Layout title="Error"><Failure error={new ApiError(404, 'ROUTE_NOT_FOUND', 'Page not found.', '')} /></Layout>} />
    </Routes>;
}
