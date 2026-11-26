import { Fragment, useEffect, useRef, type ReactNode } from 'react';
import { useBeforeUnload, useBlocker } from 'react-router';
import { ApiError } from '../../lib/api';

export type BatchConfig = {
  schema: string;
  table: string;
  key_column: string;
  value_column: string;
  delta: number;
  batch_size: number;
};
export type HeadExecutionSummary = {
  execution_id: string;
  definition_id: string;
  title: string;
  revision: string;
  irreversible: boolean;
  irreversible_reason?: string;
  can_rollback: boolean;
};
export type ForwardCandidateSummary = {
  source_execution_id: string;
  definition_id: string;
  title: string;
  revision: string;
};
export type HeadState = {
  initialized: boolean;
  head_generation: string;
  redo_depth: number;
  head: HeadExecutionSummary | null;
  next_forward: ForwardCandidateSummary | null;
};

export function batchTemplateSQL(schema: string, table: string, keyColumn: string, valueColumn: string) {
  const q = (ident: string) => `"${ident.trim().replaceAll('"', '""')}"`;
  const target = `${q(schema)}.${q(table)}`;
  const val = q(valueColumn);
  const key = q(keyColumn);
  return {
    upSQL: `UPDATE ${target} SET ${val}=${val}+$2 WHERE ${key}=ANY($1)`,
    downSQL: `UPDATE ${target} SET ${val}=${val}-$2 WHERE ${key}=ANY($1)`,
  };
}

const batchUpPattern = /^UPDATE "([^"]+)"\."([^"]+)" SET "([^"]+)"="([^"]+)"\+\$2 WHERE "([^"]+)"=ANY\(\$1\)$/;

export function parseBatchTemplateSQL(sql?: string): { schema: string; table: string; keyColumn: string; valueColumn: string } | null {
  if (!sql) return null;
  const match = batchUpPattern.exec(sql.trim());
  if (!match || match[3] !== match[4]) return null;
  return { schema: match[1], table: match[2], valueColumn: match[3], keyColumn: match[5] };
}

export type Draft = { id: string; title: string; sql?: string; down_sql?: string; irreversible?: boolean; irreversible_reason?: string; frozen?: boolean; revision: string; updated_by: string; updated_at: string };
export type RequestDecision = { actor: string; leader_id: string; created_at: string };
export type RequestGroup = { group_id: string; group_name: string; access: 'view_only' | 'view_edit'; leader_id: string; leader_username: string; decision?: RequestDecision; eligible: boolean };
export type SubmittedRequest = {
  id: string; draft_id: string; definition_id: string; title: string; actor_id: string; actor: string; revision: string;
  direction: string; mode: string; state: string; stale_reason?: string; created_at: string; updated_at: string;
  sql?: string; down_sql?: string; irreversible: boolean; irreversible_reason?: string;
  config_revision: string; policy_generation: string; head_generation: string; expected_head_id: string | null;
  groups?: RequestGroup[]; eligible_group_ids?: string[]; execution_id?: string | null; job_id?: string | null; consumed_at?: string | null;
  batch?: BatchConfig;
};
export type CommittedEvent = {
  id: string; execution_id: string; definition_id: string; title: string; actor: string; revision: string;
  direction: string; mode: string; status: string; plan_hash: string; parent_execution_id: string | null;
  canceled_execution_id: string | null; generation: string; committed_at: string;
  sql?: string; down_sql?: string; irreversible: boolean; irreversible_reason?: string;
};
export type ActivityItem = {
  source_kind: 'legacy_attempt' | 'execution' | 'worker_attempt'; id: string; execution_id?: string;
  attempt_id?: string; attempt_number?: number; title: string; actor: string; state: string;
  revision: string; created_at: string;
};
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

export function draftError(error: unknown, title: string, sql: string, downSQL = '', reason = '') {
  if (error instanceof ApiError && error.code === 'DRAFT_FROZEN') {
    return new Error('This draft is frozen after submission or execution. Create a new draft to make changes.');
  }
  if (!(error instanceof ApiError) || error.code !== 'VALIDATION_FAILED') return error;
  const message = !title.trim() || [...title].length > 120 || title.includes('\0')
    ? 'Title must contain 1–120 characters, with nonblank text and no NUL.'
    : !sql || new TextEncoder().encode(sql).length > 128 * 1024 || sql.includes('\0')
      ? 'Up SQL must contain 1 byte to 128 KiB of UTF-8 text without NUL. Invalid SQL syntax may be saved.'
      : downSQL && (new TextEncoder().encode(downSQL).length > 128 * 1024 || downSQL.includes('\0'))
        ? 'Down SQL may contain at most 128 KiB of UTF-8 text without NUL.'
        : [...reason].length > 500 || reason.includes('\0')
          ? 'Irreversible reason may contain at most 500 characters without NUL.' : error.message;
  return new Error(message);
}

export function DefinitionPreview({ draft }: { draft: Draft }) {
  return <>
    <h2>Up migration</h2>
    <SQL sql={draft.sql ?? ''} />
    <h2>Down migration</h2>
    {draft.irreversible
      ? <p className="callout"><strong>Irreversible.</strong> {draft.irreversible_reason || 'No reason recorded.'}</p>
      : <SQL sql={draft.down_sql ?? ''} />}
    {draft.frozen && <p className="note">This definition is frozen. Create a new draft to change it.</p>}
  </>;
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
