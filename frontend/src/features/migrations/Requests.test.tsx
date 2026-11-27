import { describe, expect, it } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { RequestGroups } from './Requests';
import type { RequestGroup } from './shared';

const base: RequestGroup = { group_id: 'g1', group_name: 'Editors', access: 'view_edit', leader_id: 'u1', leader_username: 'leader_one', eligible: false };

describe('review request groups', () => {
  it('shows the recorded decision and no action for a completed group', () => {
    const html = renderToStaticMarkup(<RequestGroups groups={[{ ...base, decision: {
      actor: 'leader_one', leader_id: 'u1', created_at: '2026-11-25T07:00:00Z',
    } }]} />);
    expect(html).toContain('leader_one');
    expect(html).toContain('2026-11-25 07:00:00');
    expect(html).toContain('Recorded');
    expect(html).not.toContain('Approve for');
  });
  it('offers exactly the eligible group action and waits for others', () => {
    const html = renderToStaticMarkup(<RequestGroups groups={[
      { ...base, eligible: true },
      { ...base, group_id: 'g2', group_name: 'Viewers', access: 'view_only', leader_id: 'u2', leader_username: 'leader_two' },
    ]} onApprove={() => {}} />);
    expect(html).toContain('Approve for Editors');
    expect(html).toContain('Waiting for leader');
    expect(html).not.toContain('Approve for Viewers');
  });
});
