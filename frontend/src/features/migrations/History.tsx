import { useState } from 'react';
import { Link, useNavigate } from 'react-router';
import { Pager } from '../../app/ui';
import type { Page } from '../../lib/api';
import { SQL, State, utc, type Attempt } from './shared';

export function History({ base, attempts, page, searchKey }: { base: string; attempts: Attempt[]; page?: Page; searchKey: string }) {
  const [key, setKey] = useState(searchKey);
  const navigate = useNavigate();
  return <>
    <form className="searchbar" method="get" action={`${base}/migrations`} onSubmit={(event) => {
      event.preventDefault(); navigate(`${base}/migrations?${new URLSearchParams({ request_key: key })}`);
    }}>
      <label htmlFor="request_key">Find an attempt by request key</label>
      <input id="request_key" name="request_key" value={key} onChange={(event) => setKey(event.target.value)} spellCheck={false} />
      <button type="submit">Find attempt</button>
    </form>
    {attempts.length ? <>
      <div className="table-wrap"><table>
        <thead><tr><th>Version</th><th>Draft</th><th>Revision</th><th>Run by</th><th>Created (UTC)</th><th>Result</th></tr></thead>
        <tbody>{attempts.map((attempt) => <tr key={attempt.id}>
          <td><Link to={`${base}/migrations/${attempt.id}`}>{attempt.version}</Link></td>
          <td>{attempt.title}</td><td>{attempt.revision}</td><td>{attempt.actor}</td><td>{utc(attempt.created_at)}</td><td><State state={attempt.state} /></td>
        </tr>)}</tbody>
      </table></div>
      <Pager page={page} />
    </> : searchKey ? <p>None of your attempts on this database has that request key.</p> : <p>Nothing has been run on this database yet.</p>}
  </>;
}

export function AttemptResult({ base, attempt: a, refresh }: { base: string; attempt: Attempt; refresh: () => void }) {
  return <>
    <div className={`verdict ${a.state}`} aria-hidden="true"><span className="verdict-ring" /><span className="verdict-word">{a.state}</span></div>
    <p className={`status ${a.state}`} role="status">
      <State state={a.state} />
      {a.state === 'succeeded' ? ' The migration was committed to the database.' : a.state === 'failed' ? ' The migration was not applied. Nothing was committed.' : a.state === 'unknown'
        ? ' The result is not confirmed. New executions on this database stay blocked until an operator checks the outcome.'
        : <> The migration is still in progress. <Link to={`${base}/migrations/${a.id}`} onClick={(event) => { event.preventDefault(); refresh(); }}>Refresh status</Link></>}
    </p>
    {a.failure && <dl className="meta">
      <dt>Reason</dt><dd><code>{a.failure.code}</code>{` ${a.failure.message}`}</dd>
      {a.failure.sqlstate && <><dt>SQLSTATE</dt><dd><code>{a.failure.sqlstate}</code></dd></>}
      {!!a.failure.statement_index && <><dt>Statement</dt><dd>{a.failure.statement_index}</dd></>}
    </dl>}
    <h2>Details</h2>
    <dl className="meta">
      <dt>Version</dt><dd>{a.version}</dd>
      {a.target_name && <><dt>Database snapshot</dt><dd>{a.target_name}</dd></>}
      <dt>Draft</dt><dd>{`${a.title}, revision ${a.revision}`}</dd>
      <dt>Run by</dt><dd>{a.actor}</dd>
      <dt>Created</dt><dd>{`${utc(a.created_at)} UTC`}</dd>
      {a.started_at && <><dt>Started</dt><dd>{`${utc(a.started_at)} UTC`}</dd></>}
      {a.commit_started_at && <><dt>Commit started</dt><dd>{`${utc(a.commit_started_at)} UTC`}</dd></>}
      {a.finished_at && <><dt>Finished</dt><dd>{`${utc(a.finished_at)} UTC`}</dd></>}
      {a.first_unknown_at && <><dt>Marked unknown</dt><dd>{`${utc(a.first_unknown_at)} UTC`}</dd></>}
      {a.resolved_at && <><dt>Resolved by operator</dt><dd>{`${utc(a.resolved_at)} UTC`}</dd></>}
      {a.resolution_summary && <><dt>Operator summary</dt><dd>{a.resolution_summary}</dd></>}
      <dt>SQL SHA-256</dt><dd><code>{a.sql_hash}</code></dd>
    </dl>
    <h2>SQL that ran</h2>
    <SQL sql={a.sql ?? ''} />
    <p className="back"><Link to={`${base}/migrations`}>Back to history</Link></p>
  </>;
}
