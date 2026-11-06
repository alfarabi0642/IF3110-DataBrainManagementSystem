import { describe, expect, it } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router';
import { AttemptResult } from './History';
import { SQL, type Attempt } from './shared';

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
});
