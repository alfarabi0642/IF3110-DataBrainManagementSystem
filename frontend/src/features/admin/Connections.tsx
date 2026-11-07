import { useRef, useState } from 'react';
import { Link, useNavigate } from 'react-router';
import { ApiError, type Page } from '../../lib/api';
import type { Session } from '../../lib/session';
import { Pager } from '../../app/ui';
import { FormError, SSLMode, StatusField, status, submit, takePassword, text, useAdminAction, type Connection, type Probe } from './shared';

function ConnectionFields({ connection }: { connection?: Connection }) {
  return <>
    <label htmlFor="name">Name</label><input id="name" name="name" maxLength={80} required defaultValue={connection?.name} />
    {!connection && <>
      <label htmlFor="host">Host</label><input id="host" name="host" maxLength={253} required />
      <label htmlFor="port">Port</label><input id="port" name="port" type="number" min={1} max={65535} required />
      <label htmlFor="database_name">Database name</label><input id="database_name" name="database_name" maxLength={63} required />
    </>}
    <label htmlFor="username">Username</label><input id="username" name="username" maxLength={63} required autoComplete="off" defaultValue={connection?.username} />
    <label htmlFor="password">{connection ? 'New password' : 'Password'}</label><input id="password" name="password" type="password" required={!connection} autoComplete="new-password" />
    <p className="muted">{connection ? 'Leave empty to keep the stored password.' : 'The password is stored encrypted and is never shown again.'}</p>
    <SSLMode value={connection?.ssl_mode} />
  </>;
}

export function Connections({ connections, page, session }: { connections: Connection[]; page?: Page; session: Session }) {
  const form = useRef<HTMLFormElement>(null);
  const navigate = useNavigate();
  const create = useAdminAction<Connection>(session, '/admin/databases', () => ({ name: text(form.current, 'name'), host: text(form.current, 'host'),
    port: Number(text(form.current, 'port')), database_name: text(form.current, 'database_name'), username: text(form.current, 'username'),
    password: takePassword(form.current), ssl_mode: text(form.current, 'ssl_mode'),
  }), ({ id }) => navigate(`/admin/databases/${id}`));
  return <>
    <FormError error={create.error} code />
    {connections.length ? <><div className="table-wrap"><table><thead><tr><th>Name</th><th>Server</th><th>Database</th><th>User</th><th>SSL mode</th><th>Status</th></tr></thead><tbody>{connections.map((item) => <tr key={item.id}><td><Link to={`/admin/databases/${item.id}`}>{item.name}</Link></td><td>{`${item.host}:${item.port}`}</td><td>{item.database_name}</td><td>{item.username}</td><td>{item.ssl_mode}</td><td>{status(item.is_active)}</td></tr>)}</tbody></table></div><Pager page={page} /></> : <p>No connections yet. Add the first one below.</p>}
    <h2>New connection</h2>
    <form ref={form} method="post" action="/admin/databases" className="panel form-panel" onSubmit={submit(create)}>
      <ConnectionFields /><button type="submit" disabled={create.isPending}>Test and add connection</button>
    </form>
    <p className="muted">The server checks the connection before saving. A failed check saves nothing.</p>
  </>;
}

const reasons: Record<string, string> = {
  authentication_failed: 'the server rejected the username or password.',
  connection_refused: 'nothing accepted the connection at that host and port.',
  timeout: 'the server did not answer in time.',
  tls_failed: 'the TLS handshake or certificate check failed.',
  unsupported_version: 'the server version is not supported.',
  unsafe_role: 'the database user has more privileges than allowed.',
  platform_target_forbidden: 'this is the platform database, which cannot be a target.',
};

export function ConnectionProfile({ connection, session, saved }: { connection: Connection; session: Session; saved: () => void }) {
  const form = useRef<HTMLFormElement>(null);
  const revision = useRef(connection.config_revision);
  const [probe, setProbe] = useState<Probe>();
  const base = `/admin/databases/${connection.id}`;
  const edit = useAdminAction<Connection>(session, base, () => {
    // Pertahankan revision yang dipilih saat overwrite untuk retry setelah error.
    if (conflict) revision.current = connection.config_revision;
    return { expected_revision: revision.current,
      name: text(form.current, 'name'), username: text(form.current, 'username'), password: takePassword(form.current),
      ssl_mode: text(form.current, 'ssl_mode'), is_active: text(form.current, 'is_active') === 'true',
    };
  }, saved);
  const test = useAdminAction<Probe>(session, `${base}/test`, () => ({}), setProbe);
  const conflict: boolean = edit.error instanceof ApiError && edit.error.code === 'REVISION_CONFLICT';
  return <>
    {conflict ? <p className="status failed" role="alert">Someone changed this connection. Your changes were not saved. Compare your input below with the current values, then save again to overwrite.</p> : <FormError error={edit.error ?? test.error} code />}
    {probe && (probe.ok ? <p className="status succeeded" role="status"><strong>Connection works.</strong> PostgreSQL server version number {probe.server_version}, answered in {probe.latency}.</p> : <p className="status failed" role="status"><strong>Connection failed:</strong>{' '}{reasons[probe.reason] ?? 'the server could not be reached.'}{' '}<code>{probe.reason}</code></p>)}
    <h2>Current values</h2>
    <dl className="meta">
      <dt>Name</dt><dd>{connection.name}</dd><dt>Host</dt><dd>{connection.host}</dd><dt>Port</dt><dd>{connection.port}</dd><dt>Database name</dt><dd>{connection.database_name}</dd><dt>Username</dt><dd>{connection.username}</dd><dt>SSL mode</dt><dd>{connection.ssl_mode}</dd><dt>Status</dt><dd>{status(connection.is_active)}</dd><dt>Password</dt><dd>{connection.password_stored ? 'Stored' : 'Not stored'}</dd><dt>Revision</dt><dd>{connection.config_revision}</dd><dt>Updated</dt><dd>{connection.updated_at.slice(0, 19).replace('T', ' ')} UTC</dd>
    </dl>
    <form method="post" action={`${base}/test`} onSubmit={submit(test)}><button type="submit" className="quiet" disabled={test.isPending || edit.isPending}>Test connection</button></form>
    <h2>Edit connection</h2>
    <p className="muted">Host, port and database name cannot be changed. Add a new connection for another server.</p>
    <form ref={form} method="post" action={base} className="panel form-panel" onSubmit={submit(edit)}>
      <input type="hidden" name="expected_revision" value={conflict ? connection.config_revision : revision.current} />
      <ConnectionFields connection={connection} /><StatusField active={connection.is_active} />
      <button type="submit" disabled={edit.isPending || test.isPending}>{conflict ? 'Save over the current version' : 'Save changes'}</button>
    </form>
    <p className="back"><Link to="/admin/databases">Back to connections</Link></p>
  </>;
}
