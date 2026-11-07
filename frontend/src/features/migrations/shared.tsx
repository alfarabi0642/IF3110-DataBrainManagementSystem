import { Fragment, useEffect, useRef, type ReactNode } from 'react';
import { useBeforeUnload, useBlocker } from 'react-router';
import { ApiError } from '../../lib/api';

export type Draft = { id: string; title: string; sql?: string; revision: string; updated_by: string; updated_at: string };
export type Receipt = { id: string; target_id: string; draft_id: string; revision: string; request_key: string; version: string; state: string };
export type Attempt = {
  id: string; version: string; title: string; target_name: string; revision: string; actor: string; state: string;
  created_at: string; started_at: string | null; finished_at: string | null; sql?: string; sql_hash?: string;
  commit_started_at?: string; first_unknown_at?: string; resolved_at?: string; resolution_summary?: string;
  failure?: { code: string; message: string; sqlstate: string; statement_index: number };
};

export const utc = (value: string) => value.replace('T', ' ').replace(/(?:\.\d+)?Z$/, '');
export function State({ state }: { state: string }) {
  return <span className={`badge ${state}`}>{state}</span>;
}

export function FormError({ error }: { error: unknown }) {
  return error ? <p className="status failed" role="alert">{error instanceof Error ? error.message : 'Internal server error.'}</p> : null;
}

export function draftError(error: unknown, title: string, sql: string) {
  if (!(error instanceof ApiError) || error.code !== 'VALIDATION_FAILED') return error;
  const message = !title.trim() || [...title].length > 120 || title.includes('\0')
    ? 'Title must contain 1–120 characters, with nonblank text and no NUL.'
    : !sql || new TextEncoder().encode(sql).length > 128 * 1024 || sql.includes('\0')
      ? 'SQL must contain 1 byte to 128 KiB of UTF-8 text without NUL. Invalid SQL syntax may be saved.' : error.message;
  return new Error(message);
}

export function useUnsaved(dirty: boolean) {
  const leaving = useRef(false);
  const blocker = useBlocker(() => dirty && !leaving.current);
  useEffect(() => {
    if (blocker.state !== 'blocked') return;
    if (window.confirm('You have unsaved changes. Leave this page?')) blocker.proceed();
    else blocker.reset();
  }, [blocker]);
  useBeforeUnload((event) => {
    if (dirty && !leaving.current) { event.preventDefault(); event.returnValue = ''; }
  });
  useEffect(() => {
    const guard = (event: SubmitEvent) => {
      if (dirty && event.target instanceof HTMLFormElement && event.target.getAttribute('action') === '/logout' &&
          !window.confirm('You have unsaved changes. Leave this page?')) {
        event.preventDefault(); event.stopPropagation();
      }
    };
    document.addEventListener('submit', guard, true);
    return () => document.removeEventListener('submit', guard, true);
  }, [dirty]);
  return () => { leaving.current = true; };
}

const keywords = new Set((
  'add all alter and as asc begin between by cascade case check column comment commit constraint create cross ' +
  'default deferrable delete desc distinct do drop else end exists extension false foreign from full function ' +
  'grant group having if ilike in index inner insert intersect into is join key left like limit materialized not ' +
  'null nulls offset on or order outer owner primary references rename replace restrict returning revoke right ' +
  'rollback schema select sequence set table then to transaction trigger true truncate type union unique update ' +
  'using values view when where with ' +
  'bigint bigserial boolean bool bytea char date decimal double float int integer interval json jsonb numeric ' +
  'real serial smallint text time timestamp timestamptz uuid varchar'
).split(' '));
const tokens = /(--[^\n]*|\/\*[\s\S]*?\*\/)|('(?:[^']|'')*'?)|(\b\d+(?:\.\d+)?\b)|([A-Za-z_][A-Za-z0-9_]*)/g;

export function SQL({ sql }: { sql: string }) {
  const result: ReactNode[] = [];
  let last = 0;
  if (sql.length <= 200000) for (const match of sql.matchAll(tokens)) {
    const cls = match[1] ? 'tok-com' : match[2] ? 'tok-str' : match[3] ? 'tok-num' : keywords.has(match[4].toLowerCase()) ? 'tok-kw' : '';
    if (!cls) continue;
    result.push(sql.slice(last, match.index), <span key={match.index} className={cls}>{match[0]}</span>);
    last = match.index + match[0].length;
  }
  result.push(sql.slice(last));
  return <pre className="sql">{result.map((node, index) => <Fragment key={index}>{node}</Fragment>)}</pre>;
}
