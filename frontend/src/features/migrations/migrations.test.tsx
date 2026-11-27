import { describe, expect, it } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { createMemoryRouter, MemoryRouter, RouterProvider } from 'react-router';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { DraftEditor } from './Drafts';
import type { Session } from '../../lib/session';
import { AttemptResult } from './History';
import { DefinitionPreview, draftError, SQL, type Attempt, type Draft } from './shared';
import { ApiError } from '../../lib/api';

const attempt: Attempt = { id: 'snapshot', title: 'Original <draft>', target_name: 'Original target', actor: 'original_actor',
  version: '9007199254740993', revision: '9223372036854775807', state: 'unknown', created_at: '2026-11-06T00:00:00Z',
  started_at: null, finished_at: null, sql: "-- <script>\nINSERT INTO original VALUES ('a;b', 123);", sql_hash: 'exact-hash',
  first_unknown_at: '2026-11-06T00:01:00Z', failure: { code: 'COMMIT_UNCERTAIN', message: 'The transaction outcome could not be confirmed.', sqlstate: '', statement_index: 0 } };

describe('migration results', () => {
  const render = (value: Attempt) => renderToStaticMarkup(<MemoryRouter><AttemptResult base="/databases/target" attempt={value} refresh={() => {}} /></MemoryRouter>);
  it('keeps unknown honest, immutable snapshots exact and bigint values as text', () => {
    const html = render(attempt);
    expect(html).toContain('The result is not confirmed.');
    expect(html).toContain('stay blocked until an operator checks the outcome.');
    expect(html).toContain('Original &lt;draft&gt;, revision 9223372036854775807');
    expect(html).toContain('9007199254740993');
    expect(html).toContain('Original target');
    expect(html).toContain('original_actor');
    expect(html).toContain('exact-hash');
    expect(html).not.toContain('Nothing was committed');
    expect(html).not.toContain('<dt>Finished</dt>');
    expect(html).not.toContain('<form');
  });
  it('distinguishes confirmed rollback from commit and keeps failure evidence', () => {
    const failed = render({ ...attempt, state: 'failed', failure: { code: 'SQL_ERROR', message: 'The target rejected a migration statement or transaction commit.', sqlstate: '23505', statement_index: 2 } });
    expect(failed).toContain('Nothing was committed.');
    expect(failed).toContain('<dt>SQLSTATE</dt><dd><code>23505</code>');
    expect(failed).toContain('<dt>Statement</dt><dd>2</dd>');
    const succeeded = render({ ...attempt, state: 'succeeded', failure: undefined });
    expect(succeeded).toContain('The migration was committed to the database.');
    expect(succeeded).not.toContain('Nothing was committed.');
  });
  it('highlights SQL as text without interpreting markup or splitting a quoted semicolon', () => {
    const html = renderToStaticMarkup(<SQL sql={attempt.sql!} />);
    expect(html).toContain('<span class="tok-com">-- &lt;script&gt;</span>');
    expect(html).toContain('<span class="tok-str">&#x27;a;b&#x27;</span>');
    expect(html).not.toContain('<script>');
  });
  it('renders up and down SQL with the frozen notice', () => {
    const draft: Draft = { id: 'd', title: 'Definition', sql: 'CREATE TABLE public.a(id int)',
      down_sql: 'DROP TABLE public.a', irreversible: false, frozen: true, revision: '1', updated_by: 'editor', updated_at: '2026-11-06T00:00:00Z' };
    const html = renderToStaticMarkup(<DefinitionPreview draft={draft} />);
    expect(html).toContain('Up migration');
    expect(html).toContain('Down migration');
    expect(html).toContain('This definition is frozen.');
    const text = html.replace(/<[^>]*>/g, '');
    expect(text).toContain('CREATE TABLE public.a');
    expect(text).toContain('DROP TABLE public.a');
  });
  it('renders an irreversible reason without inventing down SQL', () => {
    const draft: Draft = { id: 'd', title: 'Irreversible', sql: 'CREATE TABLE public.a(id int)',
      down_sql: '', irreversible: true, irreversible_reason: 'No safe down for this data.', revision: '1',
      updated_by: 'editor', updated_at: '2026-11-06T00:00:00Z' };
    const html = renderToStaticMarkup(<DefinitionPreview draft={draft} />);
    expect(html).toContain('Irreversible.');
    expect(html).toContain('No safe down for this data.');
    expect(html).toContain('Down migration');
  });
  it('renders a frozen draft as read-only with copy-to-new', () => {
    const session: Session = { state: 'authenticated', user: { id: 'u', username: 'editor', display_name: 'Editor', is_platform_admin: false },
      csrf_token: 'csrf', absolute_expires_at: '2026-11-06T00:00:00Z' };
    const draft: Draft = { id: 'd', title: 'Frozen', sql: 'CREATE TABLE public.a(id int)', down_sql: '',
      irreversible: true, irreversible_reason: 'No safe down.', frozen: true, revision: '1', updated_by: 'editor', updated_at: '2026-11-06T00:00:00Z' };
    const router = createMemoryRouter([{ path: '*', element: <DraftEditor base="/databases/t/drafts" draft={draft} session={session} /> }]);
    const html = renderToStaticMarkup(<QueryClientProvider client={new QueryClient()}><RouterProvider router={router} /></QueryClientProvider>);
    expect(html).toContain('This draft is frozen after submission or execution.');
    expect(html).toContain('Copy to a new draft');
    expect(html).toContain('No safe down.');
    expect(html).not.toContain('Save revision');
  });
  it('maps a frozen draft and invalid down/reason to safe messages', () => {
    const message = (error: unknown) => error instanceof Error ? error.message : '';
    expect(message(draftError(new ApiError(409, 'DRAFT_FROZEN', 'raw driver text', ''), 'Title', 'SELECT 1')))
      .toContain('frozen after submission or execution');
    expect(message(draftError(new ApiError(422, 'VALIDATION_FAILED', 'raw', ''), 'Title', 'SELECT 1',
      'x'.repeat(128 * 1024 + 1), ''))).toContain('Down SQL');
    expect(message(draftError(new ApiError(422, 'VALIDATION_FAILED', 'raw', ''), 'Title', 'SELECT 1',
      '', 'x'.repeat(501)))).toContain('500 characters');
  });
});
