import { useRef } from 'react';
import { Link, useNavigate } from 'react-router';
import { ApiError, type Page } from '../../lib/api';
import type { Session } from '../../lib/session';
import { Pager } from '../../app/ui';
import { FormError, StatusField, status, submit, text, useAdminAction, type Group, type GroupDetail } from './shared';

export function Groups({ groups, page, session }: { groups: Group[]; page?: Page; session: Session }) {
  const form = useRef<HTMLFormElement>(null);
  const navigate = useNavigate();
  const create = useAdminAction(session, '/admin/groups', () => ({ name: text(form.current, 'name') }), ({ id }) => navigate(`/admin/groups/${id}`));
  return <>
    <p className="page-intro">Groups combine membership with database access grants.</p>
    {groups.length ? <><div className="table-wrap"><table><caption className="sr-only">Access groups</caption><thead><tr><th>Group</th><th>Status</th><th>Revision</th></tr></thead><tbody>{groups.map((group) => <tr key={group.id}><td><Link to={`/admin/groups/${group.id}`}>{group.name}</Link></td><td>{status(group.is_active)}</td><td>{group.revision}</td></tr>)}</tbody></table></div><Pager page={page} /></> : <p>No groups yet.</p>}
    <h2>Create group</h2>
    <FormError error={create.error} />
    <form ref={form} method="post" action="/admin/groups" className="panel form-panel" onSubmit={submit(create)}>
      <label htmlFor="name">Name</label><input id="name" name="name" maxLength={64} autoComplete="off" required />
      <button type="submit" disabled={create.isPending}>Create group</button>
    </form>
  </>;
}

function Remove({ path, session, saved }: { path: string; session: Session; saved: () => void }) {
  const remove = useAdminAction(session, path, () => ({}), saved);
  return <><FormError error={remove.error} /><form className="table-action" method="post" action={path} onSubmit={submit(remove)}><button type="submit" className="quiet danger-text" disabled={remove.isPending}>Remove</button></form></>;
}

export function GroupProfile({ group, session, saved }: { group: GroupDetail; session: Session; saved: () => void }) {
  const form = useRef<HTMLFormElement>(null);
  const memberForm = useRef<HTMLFormElement>(null);
  const grantForm = useRef<HTMLFormElement>(null);
  const revision = useRef(group.revision);
  const base = `/admin/groups/${group.id}`;
  const edit = useAdminAction(session, base, () => ({ expected_revision: revision.current, name: text(form.current, 'name'), is_active: text(form.current, 'is_active') === 'true' }), saved);
  const member = useAdminAction(session, `${base}/members`, () => ({ user_id: text(memberForm.current, 'user_id') }), saved);
  const grant = useAdminAction(session, `${base}/databases`, () => ({ database_id: text(grantForm.current, 'database_id'), access_level: text(grantForm.current, 'access_level') }), saved);
  const error = edit.error ?? member.error ?? grant.error;
  const conflict = error instanceof ApiError && error.code === 'REVISION_CONFLICT';
  return <>
    {conflict ? <p className="status failed" role="alert">This group changed before your update was saved. <a href={base}>Open the latest version</a> and try again.</p> : <FormError error={error} />}
    <dl className="meta"><dt>Name</dt><dd>{group.name}</dd><dt>Status</dt><dd>{status(group.is_active)}</dd><dt>Revision</dt><dd>{group.revision}</dd></dl>
    <h2>Edit group</h2>
    <form ref={form} method="post" action={base} className="panel form-panel" onSubmit={submit(edit)}>
      <input type="hidden" name="expected_revision" value={revision.current} />
      <label htmlFor="name">Name</label><input id="name" name="name" maxLength={64} autoComplete="off" required defaultValue={group.name} />
      <StatusField active={group.is_active} />
      <p className="form-help">Deactivating a group signs out its members. Reactivating it requires them to sign in again.</p>
      <button type="submit" disabled={edit.isPending}>Save group</button>
    </form>
    <h2>Members</h2>
    {group.members.length ? <div className="table-wrap"><table><caption className="sr-only">Current group members</caption><thead><tr><th>Username</th><th>Display name</th><th>Status</th><th>Action</th></tr></thead><tbody>{group.members.map((user) => <tr key={user.id}>
      <td><Link to={`/admin/users/${user.id}`}>{user.username}</Link></td><td>{user.display_name}</td><td>{status(user.is_active)}</td><td><Remove path={`${base}/members/${user.id}/remove`} session={session} saved={saved} /></td>
    </tr>)}</tbody></table></div> : <p>This group has no members.</p>}
    {!group.is_active ? <p className="muted">No active users can be added while this group is inactive. Activate it first; existing members can still be removed.</p> : group.member_choices.length ? <form ref={memberForm} method="post" action={`${base}/members`} className="panel form-panel compact-form" onSubmit={submit(member)}>
      <label htmlFor="user_id">Add active user</label><select id="user_id" name="user_id" required defaultValue=""><option value="">Choose a user</option>{group.member_choices.map((user) => <option key={user.id} value={user.id}>{user.display_name} ({user.username})</option>)}</select>
      <button type="submit" disabled={member.isPending}>Add member</button>
    </form> : <p className="muted">No active users are available to add.</p>}
    <h2>Database grants</h2>
    {group.grants.length ? <div className="table-wrap"><table><caption className="sr-only">Current database grants</caption><thead><tr><th>Database</th><th>Access</th><th>Action</th></tr></thead><tbody>{group.grants.map((item) => <tr key={item.database_id}>
      <td>{item.database_name}</td><td>{item.access === 'view_edit' ? 'View and edit' : 'View only'}</td><td><Remove path={`${base}/databases/${item.database_id}/remove`} session={session} saved={saved} /></td>
    </tr>)}</tbody></table></div> : <p>This group has no database grants.</p>}
    {group.database_choices.length ? <form ref={grantForm} method="post" action={`${base}/databases`} className="panel form-panel compact-form" onSubmit={submit(grant)}>
      <label htmlFor="database_id">Set database grant</label><select id="database_id" name="database_id" required defaultValue=""><option value="">Choose a database</option>{group.database_choices.map((item) => <option key={item.id} value={item.id}>{item.name}</option>)}</select>
      <label htmlFor="access_level">Access</label><select id="access_level" name="access_level" required><option value="view_only">View only</option><option value="view_edit">View and edit</option></select>
      {!group.is_active && <p className="form-help">While this group is inactive, only grant removal or a downgrade is accepted. Activate it before adding or upgrading access.</p>}
      <button type="submit" disabled={grant.isPending}>Set grant</button>
    </form> : <p className="muted">No active databases are available for a grant.</p>}
  </>;
}
