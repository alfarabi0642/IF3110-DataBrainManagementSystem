import { describe, expect, it } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router';
import { ActivityTable, CommittedEventTable, HeadControls } from './CommittedHistory';
import { batchTemplateSQL, parseBatchTemplateSQL, type ActivityItem, type CommittedEvent, type HeadState } from './shared';

const event: CommittedEvent = { id: 'e', execution_id: 'exec-1', definition_id: 'd', title: 'Add users',
  actor: 'editor', revision: '2', direction: 'apply', mode: 'normal', status: 'committed',
  plan_hash: 'a'.repeat(64), parent_execution_id: null, canceled_execution_id: null, generation: '4',
  committed_at: '2026-11-25T08:00:00Z', sql: 'CREATE TABLE public.users(id int)', down_sql: 'DROP TABLE public.users',
  irreversible: false };

describe('committed history components', () => {
  it('links the event to its frozen detail and shows generation', () => {
    const html = renderToStaticMarkup(<MemoryRouter><CommittedEventTable base="/databases/t" events={[event]} /></MemoryRouter>);
    expect(html).toContain('Add users');
    expect(html).toContain('/databases/t/history/events/exec-1');
    expect(html).toContain('<td>4</td>');
    expect(html).toContain('apply');
  });
  it('keeps failed legacy and execution activity visible', () => {
    const items: ActivityItem[] = [
      { source_kind: 'legacy_attempt', id: 'a', attempt_id: 'a', title: 'Old attempt', actor: 'editor', state: 'failed', revision: '1', created_at: '2026-11-25T08:00:00Z' },
      { source_kind: 'execution', id: 'e', execution_id: 'exec-1', title: 'New execution', actor: 'editor', state: 'finished:partial', revision: '2', created_at: '2026-11-25T09:00:00Z' },
    ];
    const html = renderToStaticMarkup(<MemoryRouter><ActivityTable base="/databases/t" activity={items} /></MemoryRouter>);
    expect(html).toContain('/databases/t/migrations/a');
    expect(html).toContain('/databases/t/executions/exec-1');
    expect(html).toContain('failed');
    expect(html).toContain('finished:partial');
    expect(html).not.toContain('SQL');
  });
  it('renders rollback and forward controls for editors and hides mutations for viewers', () => {
    const state: HeadState = {
      initialized: true,
      head_generation: '6',
      redo_depth: 1,
      head: { execution_id: 'exec-1', definition_id: 'def-1', title: 'Add users', revision: '2', irreversible: false, can_rollback: true },
      next_forward: { source_execution_id: 'exec-2', definition_id: 'def-2', title: 'Add orders', revision: '1' },
    };
    const editorHTML = renderToStaticMarkup(<MemoryRouter><HeadControls base="/databases/t" headState={state} canEdit /></MemoryRouter>);
    expect(editorHTML).toContain('Request rollback: Add users');
    expect(editorHTML).toContain('Request forward: Add orders');

    const viewerHTML = renderToStaticMarkup(<MemoryRouter><HeadControls base="/databases/t" headState={state} canEdit={false} /></MemoryRouter>);
    expect(viewerHTML).toContain('Add users');
    expect(viewerHTML).not.toContain('Request rollback');
    expect(viewerHTML).not.toContain('Request forward');

    const irrevHTML = renderToStaticMarkup(<MemoryRouter><HeadControls base="/databases/t" headState={{
      ...state,
      head: { ...state.head!, irreversible: true, can_rollback: false },
      next_forward: null,
      redo_depth: 0,
    }} canEdit /></MemoryRouter>);
    expect(irrevHTML).toContain('Active head is irreversible');
  });
  it('round-trips v1 batch SQL template generation and parsing', () => {
    const tpl = batchTemplateSQL('public', 'accounts', 'id', 'balance');
    expect(tpl.upSQL).toBe('UPDATE "public"."accounts" SET "balance"="balance"+$2 WHERE "id"=ANY($1)');
    expect(tpl.downSQL).toBe('UPDATE "public"."accounts" SET "balance"="balance"-$2 WHERE "id"=ANY($1)');
    expect(parseBatchTemplateSQL(tpl.upSQL)).toEqual({ schema: 'public', table: 'accounts', keyColumn: 'id', valueColumn: 'balance' });
    expect(parseBatchTemplateSQL('CREATE TABLE public.t(id int)')).toBeNull();
  });
});
