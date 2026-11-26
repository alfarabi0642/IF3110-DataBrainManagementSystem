import { useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { Link, useNavigate } from 'react-router';
import { Pager } from '../../app/ui';
import { apiFetch, ApiError, type Page } from '../../lib/api';
import type { Session } from '../../lib/session';
import { batchTemplateSQL, draftError, FormError, useUnsaved, utc, type Draft } from './shared';
import { SqlEditor } from './SqlEditor';

function DraftForm({ base, draft, session }: { base: string; draft?: Draft; session: Session }) {
  const navigate = useNavigate();
  const client = useQueryClient();
  const [saved, setSaved] = useState(draft);
  const [title, setTitle] = useState(draft?.title ?? '');
  const [sql, setSQL] = useState(draft?.sql ?? '');
  const [downSQL, setDownSQL] = useState(draft?.down_sql ?? '');
  const [irreversible, setIrreversible] = useState(draft?.irreversible ?? false);
  const [reason, setReason] = useState(draft?.irreversible_reason ?? '');
  const [batchSchema, setBatchSchema] = useState('public');
  const [batchTable, setBatchTable] = useState('');
  const [batchKey, setBatchKey] = useState('id');
  const [batchValue, setBatchValue] = useState('');
  const frozen = saved?.frozen ?? false;
  const allowLeave = useUnsaved(title !== (saved?.title ?? '') || sql !== (saved?.sql ?? '') ||
    downSQL !== (saved?.down_sql ?? '') || irreversible !== (saved?.irreversible ?? false) ||
    reason !== (saved?.irreversible_reason ?? ''));
  const path = draft ? `${base}/${draft.id}` : base;
  const save = useMutation({
    mutationFn: () => apiFetch<{ draft: Draft }>(`/api${path}`, { method: 'POST', csrfToken: session.csrf_token,
      body: { title, sql, down_sql: downSQL, irreversible, irreversible_reason: reason, ...(saved ? { expected_revision: saved.revision } : {}) } }),
    onSuccess: async (result) => {
      setSaved(result.data.draft);
      await client.invalidateQueries({ queryKey: ['migrations'], refetchType: 'none' });
      if (!draft) { allowLeave(); navigate(`${base}/${result.data.draft.id}`); }
    },
  });
  const copy = useMutation({
    mutationFn: () => apiFetch<{ draft: Draft }>(`/api${base}`, { method: 'POST', csrfToken: session.csrf_token,
      body: { title, sql, down_sql: downSQL, irreversible, irreversible_reason: reason } }),
    onSuccess: async (result) => {
      allowLeave();
      await client.invalidateQueries({ queryKey: ['migrations'], refetchType: 'none' });
      navigate(`${base}/${result.data.draft.id}`);
    },
  });
  const busy = save.isPending || copy.isPending;
  const conflict = save.error instanceof ApiError && save.error.code === 'REVISION_CONFLICT';
  const fillBatchTemplate = () => {
    if (!batchSchema.trim() || !batchTable.trim() || !batchKey.trim() || !batchValue.trim()) return;
    const tpl = batchTemplateSQL(batchSchema, batchTable, batchKey, batchValue);
    setSQL(tpl.upSQL);
    setDownSQL(tpl.downSQL);
    setIrreversible(false);
    setReason('');
  };
  return <>
    {frozen && <p className="status" role="status">This draft is frozen after submission or execution. Create a new draft to make changes.</p>}
    {conflict && <p className="status failed" role="alert">Someone saved a newer revision. Your changes were not saved. <Link to={path} reloadDocument>Open the latest version</Link> and apply them again.</p>}
    <FormError error={draftError(save.error ?? copy.error, title, sql, downSQL, reason)} />
    {saved && <p className="draft-meta">{`Revision ${saved.revision}, last saved by ${saved.updated_by} at ${utc(saved.updated_at)} UTC.`}</p>}
    <form method="post" action={path} onSubmit={(event) => { event.preventDefault(); if (!busy && !frozen) save.mutate(); }}>
      <input type="hidden" name="_csrf" value={session.csrf_token} />
      {saved && <input type="hidden" name="expected_revision" value={saved.revision} />}
      <label htmlFor="title">Title</label>
      <input id="title" name="title" maxLength={120} required value={title} disabled={busy || frozen} onChange={(event) => setTitle(event.target.value)} />
      {!frozen && <details>
        <summary>Fill v1 bounded batch SQL template (optional)</summary>
        <p className="note">Generates the exact parameterized Up/Down SQL required for bounded batch mode. Set delta and batch size on the Review screen.</p>
        <label htmlFor="tpl-schema">Schema</label>
        <input id="tpl-schema" value={batchSchema} disabled={busy} onChange={(e) => setBatchSchema(e.target.value)} />
        <label htmlFor="tpl-table">Table</label>
        <input id="tpl-table" value={batchTable} disabled={busy} placeholder="accounts" onChange={(e) => setBatchTable(e.target.value)} />
        <label htmlFor="tpl-key">Primary key column (bigint)</label>
        <input id="tpl-key" value={batchKey} disabled={busy} onChange={(e) => setBatchKey(e.target.value)} />
        <label htmlFor="tpl-value">Numeric value column (bigint)</label>
        <input id="tpl-value" value={batchValue} disabled={busy} placeholder="balance" onChange={(e) => setBatchValue(e.target.value)} />
        <button type="button" className="quiet" disabled={busy || !batchTable.trim() || !batchValue.trim()} onClick={fillBatchTemplate}>Fill Up &amp; Down batch SQL</button>
      </details>}
      <label htmlFor="sql">Up SQL</label>
      <SqlEditor id="sql" name="sql" filename="up.sql" value={sql} original={saved?.sql ?? ''} onChange={setSQL} disabled={busy || frozen} />
      <label className="check"><input type="checkbox" name="irreversible" checked={irreversible} disabled={busy || frozen}
        onChange={(event) => { setIrreversible(event.target.checked); if (!event.target.checked) setReason(''); }} /> Irreversible — there is no down migration</label>
      {irreversible && <>
        <label htmlFor="irreversible_reason">Irreversible reason</label>
        <textarea id="irreversible_reason" name="irreversible_reason" maxLength={500} value={reason} disabled={busy || frozen}
          onChange={(event) => setReason(event.target.value)} />
        <p className="note">Down SQL must stay empty for an irreversible definition.</p>
      </>}
      <label htmlFor="down_sql">Down SQL</label>
      <SqlEditor id="down_sql" name="down_sql" filename="down.sql" required={false} value={downSQL} original={saved?.down_sql ?? ''} onChange={setDownSQL} disabled={busy || frozen} />
      <p className="note">Down SQL is author-provided rollback SQL; the original data is not restored automatically. Leave it empty only when the migration is irreversible.</p>
      {frozen
        ? <button type="button" disabled={busy} onClick={() => copy.mutate()}>Copy to a new draft</button>
        : <button type="submit" disabled={busy}>{draft ? 'Save revision' : 'Save draft'}</button>}
    </form>
    {draft && <>
      <div className="after-form">
        <p className="note">Saving does not change the database. Review always uses the last saved revision.</p>
        <p>{!save.isError && <Link className="button" to={`${path}/review`}>Review saved revision</Link>}</p>
      </div>
      <p className="back"><Link to={base}>All drafts</Link>{' · '}<Link to={`${base.slice(0, -'/drafts'.length)}/requests`}>Review requests</Link></p>
    </>}
  </>;
}

export function Drafts({ base, drafts, page, session }: { base: string; drafts: Draft[]; page?: Page; session: Session }) {
  return <>
    <p className="page-intro">Unsubmitted drafts are editor-only. <Link to={`${base.slice(0, -'/drafts'.length)}/requests`}>Submitted review requests</Link>.</p>
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
