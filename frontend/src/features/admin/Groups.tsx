import { useRef } from 'react';
import { useQuery } from '@tanstack/react-query';
import { Link, useNavigate } from 'react-router';
import { apiFetch, ApiError, type Page } from '../../lib/api';
import type { Session } from '../../lib/session';
import { Pager } from '../../app/ui';
import { FormError, LeaderState, StatusField, status, submit, text, useAdminAction, type Group, type GroupDetail, type User } from './shared';

export function Groups({ groups, page, session }: { groups: Group[]; page?: Page; session: Session }) {
  const form = useRef<HTMLFormElement>(null);
  const navigate = useNavigate();
  const users = useQuery({ queryKey: ['admin', 'group-leader-choices'],
    queryFn: ({ signal }) => apiFetch<User[]>('/api/admin/users?limit=200', { signal }) });
  const leaders = (users.data?.data ?? []).filter((user) => user.is_active);
  const create = useAdminAction(session, '/admin/groups', () => ({
    name: text(form.current, 'name'), ...(text(form.current, 'leader_id') ? { leader_id: text(form.current, 'leader_id') } : {}),
  }), ({ id }) => navigate(`/admin/groups/${id}`));
  return <>
    <p className="page-intro">Groups combine membership with database access grants.</p>
    {groups.length ? <><div className="table-wrap"><table><caption className="sr-only">Access groups</caption><thead><tr><th>Group</th><th>Status</th><th>Leader</th><th>Revision</th></tr></thead><tbody>{groups.map((group) => <tr key={group.id}><td><Link to={`/admin/groups/${group.id}`}>{group.name}</Link></td><td>{status(group.is_active)}</td><td><LeaderState group={group} /></td><td>{group.revision}</td></tr>)}</tbody></table></div><Pager page={page} /></> : <p>No groups yet.</p>}
    <h2>Create group</h2>
    <FormError error={create.error} />
    <form ref={form} method="post" action="/admin/groups" className="panel form-panel" onSubmit={submit(create)}>
      <label htmlFor="name">Name</label><input id="name" name="name" maxLength={64} autoComplete="off" required />
      <label htmlFor="leader_id">Initial leader</label>
      <select id="leader_id" name="leader_id" defaultValue="">
        <option value="">Choose later (repair needed)</option>
        {leaders.map((user) => <option key={user.id} value={user.id}>{user.display_name} ({user.username})</option>)}
      </select>
      <p className="form-help">Selecting an active user also adds them as a member. Leadership does not grant database edit access.</p>
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
  const leaderForm = useRef<HTMLFormElement>(null);
  const revision = useRef(group.revision);
  const base = `/admin/groups/${group.id}`;
  const edit = useAdminAction(session, base, () => ({ expected_revision: revision.current, name: text(form.current, 'name'), is_active: text(form.current, 'is_active') === 'true' }), saved);
  const member = useAdminAction(session, `${base}/members`, () => ({ user_id: text(memberForm.current, 'user_id') }), saved);
  const grant = useAdminAction(session, `${base}/databases`, () => ({ database_id: text(grantForm.current, 'database_id'), access_level: text(grantForm.current, 'access_level') }), saved);
  const leader = useAdminAction<{ group: Group }>(session, `${base}/leader`, () => ({
    user_id: text(leaderForm.current, 'user_id'), expected_revision: group.revision,
  }), () => saved());
  const activeMembers = group.members.filter((user) => user.is_active);
  const error = edit.error ?? member.error ?? grant.error ?? leader.error;
  const conflict = error instanceof ApiError && error.code === 'REVISION_CONFLICT';
  return <>
    {conflict ? <p className="status failed" role="alert">This group changed before your update was saved. <a href={base}>Open the latest version</a> and try again.</p> : <FormError error={error} />}
    <dl className="meta"><dt>Name</dt><dd>{group.name}</dd><dt>Status</dt><dd>{status(group.is_active)}</dd><dt>Leader</dt><dd><LeaderState group={group} /></dd><dt>Revision</dt><dd>{group.revision}</dd></dl>
    <h2>Edit group</h2>
    <form ref={form} method="post" action={base} className="panel form-panel" onSubmit={submit(edit)}>
      <input type="hidden" name="expected_revision" value={revision.current} />
      <label htmlFor="name">Name</label><input id="name" name="name" maxLength={64} autoComplete="off" required defaultValue={group.name} />
      <StatusField active={group.is_active} />
      <p className="form-help">Deactivating a group signs out its members. Reactivating it requires them to sign in again.</p>
      <button type="submit" disabled={edit.isPending}>Save group</button>
    </form>
    <h2>Leader</h2>
    {!group.leader_id && <p className="status failed" role="alert">Repair needed: this group has no leader. Select an active member below.</p>}
    {activeMembers.length ? <form ref={leaderForm} method="post" action={`${base}/leader`} className="panel form-panel compact-form" onSubmit={submit(leader)}>
      <input type="hidden" name="expected_revision" value={group.revision} />
      <label htmlFor="leader_user_id">{group.leader_id ? 'Replace leader' : 'Assign leader'}</label>
      <select id="leader_user_id" name="user_id" required defaultValue={group.leader_id ?? ''}>
        <option value="">Choose an active member</option>
        {activeMembers.map((user) => <option key={user.id} value={user.id}>{user.display_name} ({user.username})</option>)}
      </select>
      <p className="form-help">The leader must be an active member. Leadership does not grant database edit access; every required group still votes on requests.</p>
      <button type="submit" disabled={leader.isPending}>Save leader</button>
    </form> : <p className="muted">No active members are available. Add an active member before repairing leadership.</p>}
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
