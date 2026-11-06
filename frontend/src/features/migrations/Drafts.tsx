import { useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { Link, useNavigate } from 'react-router';
import { Pager } from '../../app/ui';
import { apiFetch, ApiError, type Page } from '../../lib/api';
import type { Session } from '../../lib/session';
import { draftError, FormError, useUnsaved, utc, type Draft } from './shared';
import { SqlEditor } from './SqlEditor';

function DraftForm({ base, draft, session }: { base: string; draft?: Draft; session: Session }) {
  const navigate = useNavigate();
  const client = useQueryClient();
  const [saved, setSaved] = useState(draft);
  const [title, setTitle] = useState(draft?.title ?? '');
  const [sql, setSQL] = useState(draft?.sql ?? '');
  const allowLeave = useUnsaved(title !== (saved?.title ?? '') || sql !== (saved?.sql ?? ''));
  const path = draft ? `${base}/${draft.id}` : base;
  const save = useMutation({
    mutationFn: () => apiFetch<{ draft: Draft }>(`/api${path}`, { method: 'POST', csrfToken: session.csrf_token,
      body: { title, sql, ...(saved ? { expected_revision: saved.revision } : {}) } }),
    onSuccess: async (result) => {
      setSaved(result.data.draft);
      await client.invalidateQueries({ queryKey: ['migrations'], refetchType: 'none' });
      if (!draft) { allowLeave(); navigate(`${base}/${result.data.draft.id}`); }
    },
  });
  const conflict = save.error instanceof ApiError && save.error.code === 'REVISION_CONFLICT';
  return <>
    {conflict && <p className="status failed" role="alert">Someone saved a newer revision. Your changes were not saved. <Link to={path} reloadDocument>Open the latest version</Link> and apply them again.</p>}
    <FormError error={draftError(save.error, title, sql)} />
    {saved && <p className="draft-meta">{`Revision ${saved.revision}, last saved by ${saved.updated_by} at ${utc(saved.updated_at)} UTC.`}</p>}
    <form method="post" action={path} onSubmit={(event) => { event.preventDefault(); if (!save.isPending) save.mutate(); }}>
      <input type="hidden" name="_csrf" value={session.csrf_token} />
      {saved && <input type="hidden" name="expected_revision" value={saved.revision} />}
      <label htmlFor="title">Title</label>
      <input id="title" name="title" maxLength={120} required value={title} disabled={save.isPending} onChange={(event) => setTitle(event.target.value)} />
      <label htmlFor="sql">SQL</label>
      <SqlEditor value={sql} original={saved?.sql ?? ''} onChange={setSQL} disabled={save.isPending} />
      <button type="submit" disabled={save.isPending}>{draft ? 'Save revision' : 'Save draft'}</button>
    </form>
    {draft && <>
      <div className="after-form">
        <p className="note">Saving does not change the database. Review always uses the last saved revision.</p>
        <p>{!save.isError && <Link className="button" to={`${path}/review`}>Review saved revision</Link>}</p>
      </div>
      <p className="back"><Link to={base}>All drafts</Link></p>
    </>}
  </>;
}

export function Drafts({ base, drafts, page, session }: { base: string; drafts: Draft[]; page?: Page; session: Session }) {
  return <>
    {drafts.length ? <>
      <div className="table-wrap"><table>
        <thead><tr><th>Title</th><th>Revision</th><th>Last saved by</th><th>Saved at (UTC)</th></tr></thead>
        <tbody>{drafts.map((draft) => <tr key={draft.id}>
          <td><Link to={`${base}/${draft.id}`}>{draft.title}</Link></td><td>{draft.revision}</td><td>{draft.updated_by}</td><td>{utc(draft.updated_at)}</td>
        </tr>)}</tbody>
      </table></div>
      <Pager page={page} />
    </> : <p>No drafts yet. Write the first one below.</p>}
    <h2>New draft</h2>
    <DraftForm base={base} session={session} />
  </>;
}

export function DraftEditor({ base, draft, session }: { base: string; draft: Draft; session: Session }) {
  return <DraftForm base={base} draft={draft} session={session} />;
}
