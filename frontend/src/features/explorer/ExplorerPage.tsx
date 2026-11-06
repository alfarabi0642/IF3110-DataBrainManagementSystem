import { useQuery } from '@tanstack/react-query';
import { Link, Navigate, useLocation, useParams } from 'react-router';
import { Failure, Layout, Pager, type WorkspaceDatabase } from '../../app/ui';
import { apiFetch, ApiError } from '../../lib/api';
import type { Session } from '../../lib/session';

type Cell = { null: boolean; text: string };
type Explorer = {
  database: WorkspaceDatabase;
  schema: string;
  table: string;
  tab: string;
  schemas: string[];
  tables: { schema: string; name: string; kind: string }[];
  columns: { ordinal: number; name: string; type: string; nullable: boolean; default: string | null; identity: string; generated: string }[];
  constraints: { name: string; kind: string; columns: string[]; definition: string }[];
  indexes: { name: string; method: string; unique: boolean; primary: boolean; valid: boolean; definition: string }[];
  data: { columns: string[]; rows: Cell[][]; ordering: string } | null;
};

const tabs = [['data', 'Data'], ['columns', 'Columns'], ['constraints', 'Constraints'], ['indexes', 'Indexes']];
const kinds: Record<string, string> = { p: 'Primary key', f: 'Foreign key', u: 'Unique', c: 'Check', x: 'Exclusion', t: 'Constraint trigger' };
const yesno = (value: boolean) => value ? 'yes' : 'no';

function CellValue({ cell }: { cell: Cell }) {
  // Hanya flag null menandai SQL NULL; teks PostgreSQL tidak dikonversi.
  if (cell.null) return <span className="null">NULL</span>;
  if (cell.text === '') return <span className="empty-string">(empty)</span>;
  return cell.text;
}

export function ExplorerPage({ session }: { session: Session }) {
  const { id } = useParams();
  const { pathname, search } = useLocation();
  const query = useQuery({
    queryKey: ['explorer', session.user?.id, id, search],
    queryFn: ({ signal }) => apiFetch<Explorer>(`/api/databases/${encodeURIComponent(id ?? '')}/explorer${search}`, { signal }),
    enabled: session.state === 'authenticated',
    gcTime: 0,
  });
  if (session.state !== 'authenticated') return <Navigate to="/login" replace />;
  if (query.isPending || query.error instanceof ApiError && query.error.status === 401) {
    return <Layout session={session} title="Explorer"><p className="page-intro" role="status">Loading…</p></Layout>;
  }
  if (query.isError) {
    const beforeScope = query.error instanceof ApiError && ['INVALID_REQUEST', 'DATABASE_NOT_FOUND', 'DATABASE_ACCESS_DENIED'].includes(query.error.code);
    return <Layout session={beforeScope ? undefined : session} title="Error"><Failure error={query.error} /></Layout>;
  }
  const result = query.data.data;
  const limit = query.data.page?.limit ?? Number(new URLSearchParams(search).get('limit') ?? 50);
  const href = (schema = '', table = '', tab = 'data') => {
    const params = new URLSearchParams();
    if (schema) params.set('schema', schema);
    if (table) { params.set('table', table); params.set('tab', tab); }
    if (tab === 'data') params.set('limit', String(limit));
    return `${pathname}?${params}`;
  };
  const data = result.data;
  return <Layout session={session} title="Explorer" database={result.database}>
    {!result.schema ? <>
      <h2>Schemas</h2>
      {result.schemas.length ? <ul className="plain tiles schemas">{result.schemas.map(name => <li key={name}><Link to={href(name)}>{name}</Link></li>)}</ul> : <p>This database has no schemas you can browse.</p>}
    </> : <>
      <p className="crumbs"><Link to={href()}>All schemas</Link>{result.table ? <>{' / '}<Link to={href(result.schema)}>{result.schema}</Link>{` / ${result.table}`}</> : ` / ${result.schema}`}</p>
      {!result.table ? <>
        <h2>{`Tables in ${result.schema}`}</h2>
        {result.tables.length ? <ul className="plain tiles tables">{result.tables.map(table => <li key={table.name}><Link to={href(result.schema, table.name)}>{table.name}</Link></li>)}</ul> : <p>This schema has no tables.</p>}
      </> : <>
        <nav className="tabs" aria-label="Table views">{tabs.map(([value, label]) => <Link key={value} to={href(result.schema, result.table, value)} aria-current={value === result.tab ? 'page' : undefined}>{label}</Link>)}</nav>
        {result.tab === 'data' && data && <>
          <p className="muted form-help">{data.ordering === 'unspecified' ? 'Row order is unspecified (no primary key). Pages may change between requests.' : 'Ordered by primary key ascending. Pages may change between requests.'}</p>
          {data.rows.length ? <div className="table-wrap"><table>
            <thead><tr>{data.columns.map(name => <th key={name}>{name}</th>)}</tr></thead>
            <tbody>{data.rows.map((row, i) => <tr key={i}>{row.map((cell, j) => <td className="cell" key={j}><CellValue cell={cell} /></td>)}</tr>)}</tbody>
          </table></div> : <p>No rows on this page.</p>}
          <Pager page={query.data.page} />
        </>}
        {result.tab === 'columns' && <div className="table-wrap"><table>
          <thead><tr><th>#</th><th>Name</th><th>Type</th><th>Nullable</th><th>Default</th><th>Identity</th><th>Generated</th></tr></thead>
          <tbody>{result.columns.map(column => <tr key={column.ordinal}>
            <td>{column.ordinal}</td><td>{column.name}</td><td>{column.type}</td><td>{yesno(column.nullable)}</td>
            <td>{column.default !== null ? <code>{column.default}</code> : '-'}</td>
            <td>{({ a: 'always', d: 'by default' } as Record<string, string>)[column.identity] || '-'}</td>
            <td>{column.generated === 's' ? 'stored' : '-'}</td>
          </tr>)}</tbody>
        </table></div>}
        {result.tab === 'constraints' && (result.constraints.length ? <div className="table-wrap"><table>
          <thead><tr><th>Type</th><th>Columns</th><th>Definition</th></tr></thead>
          <tbody>{result.constraints.map(constraint => <tr key={constraint.name}><td>{kinds[constraint.kind] || constraint.kind}</td><td>{constraint.columns.join(', ')}</td><td><code>{constraint.definition}</code></td></tr>)}</tbody>
        </table></div> : <p>This table has no constraints.</p>)}
        {result.tab === 'indexes' && (result.indexes.length ? <div className="table-wrap"><table>
          <thead><tr><th>Name</th><th>Method</th><th>Unique</th><th>Primary</th><th>Valid</th><th>Definition</th></tr></thead>
          <tbody>{result.indexes.map(index => <tr key={index.name}><td>{index.name}</td><td>{index.method}</td><td>{yesno(index.unique)}</td><td>{yesno(index.primary)}</td><td>{yesno(index.valid)}</td><td><code>{index.definition}</code></td></tr>)}</tbody>
        </table></div> : <p>This table has no indexes.</p>)}
      </>}
    </>}
  </Layout>;
}
