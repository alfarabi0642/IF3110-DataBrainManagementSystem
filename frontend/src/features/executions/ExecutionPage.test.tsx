import { describe, expect, it } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { MemoryRouter } from 'react-router';
import { ExecutionDetail, executionPollInterval, type Execution } from './ExecutionPage';

const execution: Execution = {
  id: '9007199254740993', target_id: 'target-1', direction: 'apply', mode: 'normal', phase: 'queued', outcome: null,
  committed_rows: '0', committed_batches: '0', total_rows: null, has_partial_commits: false,
  reconciliation_pending: false, created_at: '2026-11-06T00:00:00Z', started_at: null, finished_at: null, actions: [],
};

const render = (value: Execution) => renderToStaticMarkup(<MemoryRouter><ExecutionDetail execution={value} /></MemoryRouter>);

describe('execution monitoring', () => {
  it('shows a queued execution without inventing progress or actions', () => {
    const html = render(execution);
    expect(html).toContain('The execution is queued.');
    expect(html).toContain('<dt>Committed rows</dt><dd>0</dd>');
    expect(html).toContain('<dt>Committed batches</dt><dd>0</dd>');
    expect(html).not.toContain('%');
    expect(html).not.toContain('<form');
    expect(html).not.toContain('Resume');
    expect(html).not.toContain('Compensate');
  });

  it('keeps bigint counters as text and stops at the known total only', () => {
    const html = render({
      ...execution, phase: 'running', committed_rows: '9007199254740993', committed_batches: '9007199254740995',
      total_rows: '9007199254740997', started_at: '2026-11-06T00:01:00Z',
    });
    expect(html).toContain('9007199254740993');
    expect(html).toContain('9007199254740995');
    expect(html).toContain('9007199254740997');
    expect(html).toContain('Counts include only committed batches.');
    expect(html).not.toContain('%');
  });

  it('distinguishes unknown/blocked from failure and shows reconciliation', () => {
    const html = render({
      ...execution, phase: 'blocked', outcome: 'unknown', reconciliation_pending: true,
      committed_batches: '2', has_partial_commits: true, started_at: '2026-11-06T00:01:00Z',
    });
    expect(html).toContain('The outcome is not confirmed.');
    expect(html).toContain('Reconciliation');
    expect(html).toContain('Pending');
    expect(html).not.toContain('The execution failed.');
  });

  it('is honest about failure before and after partial commits', () => {
    const before = render({ ...execution, phase: 'finished', outcome: 'failed', finished_at: '2026-11-06T00:02:00Z',
      failure: { code: 'SQL_ERROR', message: 'The target rejected a statement.', sqlstate: '23505' } });
    expect(before).toContain('No changes from this execution were committed.');
    expect(before).toContain('<dt>SQLSTATE</dt><dd><code>23505</code></dd>');
    const after = render({ ...execution, phase: 'finished', outcome: 'failed', has_partial_commits: true,
      committed_batches: '3', failure: { code: 'SQL_ERROR', message: 'The target rejected a statement.', sqlstate: '' } });
    expect(after).toContain('failed after some batches had committed');
    expect(after).not.toContain('No changes from this execution were committed.');
  });

  it('reports confirmed success as committed', () => {
    const html = render({ ...execution, phase: 'finished', outcome: 'succeeded', committed_rows: '10',
      finished_at: '2026-11-06T00:02:00Z' });
    expect(html).toContain('The execution committed successfully.');
    expect(html).not.toContain('The execution failed.');
  });

  it('polls according to the durable contract', () => {
    expect(executionPollInterval(undefined)).toBe(false);
    expect(executionPollInterval(execution)).toBe(2000);
    expect(executionPollInterval({ ...execution, phase: 'running' })).toBe(2000);
    expect(executionPollInterval({ ...execution, phase: 'blocked', outcome: 'unknown' })).toBe(10000);
    expect(executionPollInterval({ ...execution, phase: 'running', outcome: 'partially_reverted' })).toBe(10000);
    expect(executionPollInterval({ ...execution, phase: 'finished', outcome: 'succeeded' })).toBe(false);
  });

  it('renders server-derived recovery actions and committed batch checkpoints', () => {
    const html = renderToStaticMarkup(
      <MemoryRouter>
        <ExecutionDetail
          execution={{
            ...execution,
            mode: 'batch',
            phase: 'blocked',
            outcome: 'partially_reverted',
            committed_batches: '2',
            committed_rows: '20',
            has_partial_commits: true,
            actions: [
              { action: 'resume', enabled: false, reason_code: 'COMPENSATION_IN_PROGRESS' },
              { action: 'compensate', enabled: true, reason_code: '' },
            ],
          }}
          batches={[
            {
              batch_ordinal: '1',
              direction: 'apply',
              action_execution_id: 'exec-1',
              rows_affected: '10',
              cursor_end: '10',
              committed_at: '2026-11-06T00:01:00Z',
            },
          ]}
        />
      </MemoryRouter>,
    );
    expect(html).toContain('Recovery actions');
    expect(html).toContain('Request approved resume');
    expect(html).toContain('COMPENSATION_IN_PROGRESS');
    expect(html).toContain('Request approved reverse compensation');
    expect(html).toContain('Committed batches');
    expect(html).toContain('<code>10</code>');
  });
});
