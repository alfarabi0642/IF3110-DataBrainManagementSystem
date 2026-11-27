import { useRef } from 'react';
import { Link, useNavigate } from 'react-router';
import { ApiError, type Page } from '../../lib/api';
import type { Session } from '../../lib/session';
import { Pager } from '../../app/ui';
import { AccountType, DisplayName, FormError, StatusField, status, submit, takePassword, text, useAdminAction, type User, type UserDetail } from './shared';

export function Users({ users, page, session }: { users: User[]; page?: Page; session: Session }) {
  const form = useRef<HTMLFormElement>(null);
  const navigate = useNavigate();
  const create = useAdminAction(session, '/admin/users', () => ({
    username: text(form.current, 'username'), display_name: text(form.current, 'display_name'),
    password: takePassword(form.current), is_platform_admin: text(form.current, 'is_platform_admin') === 'true',
  }), ({ id }) => navigate(`/admin/users/${id}`));
  return <>
    <p className="page-intro">Create accounts and review active, inactive, and platform administrator access.</p>
    {users.length ? <><div className="table-wrap"><table>
      <caption className="sr-only">Platform user accounts</caption>
      <thead><tr><th>Username</th><th>Display name</th><th>Status</th><th>Platform admin</th><th>Revision</th></tr></thead>
      <tbody>{users.map((user) => <tr key={user.id}><td><Link to={`/admin/users/${user.id}`}>{user.username}</Link></td><td>{user.display_name}</td><td>{status(user.is_active)}</td><td>{user.is_platform_admin ? 'yes' : 'no'}</td><td>{user.revision}</td></tr>)}</tbody>
    </table></div><Pager page={page} /></> : <p>No users yet.</p>}
    <h2>Create user</h2>
    <FormError error={create.error} />
    <form ref={form} method="post" action="/admin/users" className="panel form-panel" onSubmit={submit(create)}>
      <label htmlFor="username">Username</label>
      <input id="username" name="username" minLength={3} maxLength={32} pattern="[a-z][a-z0-9_]{2,31}" autoComplete="off" autoCapitalize="none" spellCheck={false} aria-describedby="username-help" required />
      <p id="username-help" className="form-help">Use 3–32 lowercase letters, numbers, or underscores, beginning with a letter. It cannot be changed later.</p>
      <DisplayName />
      <label htmlFor="password">Password</label>
      <input id="password" name="password" type="password" autoComplete="new-password" aria-describedby="password-help" required />
      <p id="password-help" className="form-help">Use 12–128 UTF-8 bytes. The password is never shown again.</p>
      <AccountType />
      <p className="form-help">New platform administrators must sign in before using administrator access.</p>
      <button type="submit" disabled={create.isPending}>Create user</button>
    </form>
  </>;
}

export function UserProfile({ user, session, saved }: { user: UserDetail; session: Session; saved: () => void }) {
  const form = useRef<HTMLFormElement>(null);
  const resetForm = useRef<HTMLFormElement>(null);
  const revision = useRef(user.revision);
  const base = `/admin/users/${user.id}`;
  const edit = useAdminAction(session, base, () => ({ expected_revision: revision.current,
    display_name: text(form.current, 'display_name'), is_active: text(form.current, 'is_active') === 'true',
    is_platform_admin: text(form.current, 'is_platform_admin') === 'true',
  }), saved);
  const reset = useAdminAction(session, `${base}/password`, () => ({ expected_revision: user.revision, password: takePassword(resetForm.current) }), saved);
  const error = edit.error ?? reset.error;
  const conflict = error instanceof ApiError && error.code === 'REVISION_CONFLICT';
  return <>
    {conflict ? <p className="status failed" role="alert">This user changed before your update was saved. <a href={base}>Open the latest version</a> and try again.</p> : <FormError error={error} />}
    <dl className="meta"><dt>Username</dt><dd><code>{user.username}</code></dd><dt>Display name</dt><dd>{user.display_name}</dd><dt>Status</dt><dd>{status(user.is_active)}</dd><dt>Platform admin</dt><dd>{user.is_platform_admin ? 'yes' : 'no'}</dd><dt>Revision</dt><dd>{user.revision}</dd></dl>
    <h2>Edit user</h2>
    <p className="muted">The username is permanent.</p>
    <form ref={form} method="post" action={base} className="panel form-panel" onSubmit={submit(edit)}>
      <input type="hidden" name="expected_revision" value={revision.current} />
      <DisplayName value={user.display_name} /><StatusField active={user.is_active} /><AccountType admin={user.is_platform_admin} />
      <p className="form-help">Deactivation or administrator demotion signs this user out. Promotion or reactivation requires the user to sign in again.</p>
      <button type="submit" disabled={edit.isPending || reset.isPending}>Save user</button>
    </form>
    <h2>Reset password</h2>
    <form ref={resetForm} method="post" action={`${base}/password`} className="panel form-panel" onSubmit={submit(reset)}>
      <input type="hidden" name="expected_revision" value={user.revision} />
      <label htmlFor="password">New password</label>
      <input id="password" name="password" type="password" autoComplete="new-password" aria-describedby="password-reset-help" required />
      <p id="password-reset-help" className="form-help">Resetting the password immediately signs this user out of every session.</p>
      <button type="submit" disabled={edit.isPending || reset.isPending}>Reset password</button>
    </form>
    <h2>Groups</h2>
    {user.groups.length ? <div className="table-wrap"><table><caption className="sr-only">Groups containing this user</caption><thead><tr><th>Group</th><th>Status</th></tr></thead><tbody>{user.groups.map((group) => <tr key={group.id}><td><Link to={`/admin/groups/${group.id}`}>{group.name}</Link></td><td>{status(group.is_active)}</td></tr>)}</tbody></table></div> : <p>This user is not a member of any group.</p>}
  </>;
}
