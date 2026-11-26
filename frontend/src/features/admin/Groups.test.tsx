import { describe, expect, it } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { LeaderState, type Group } from './shared';

const group: Group = { id: 'g', name: 'Engineering', is_active: true, revision: '1' };

describe('group leadership display', () => {
  it('shows the configured leader by display name', () => {
    const html = renderToStaticMarkup(<LeaderState group={{ ...group, leader_id: 'u', leader_username: 'leader',
      leader_display_name: 'Lead <User>' }} />);
    expect(html).toContain('Lead &lt;User&gt;');
    expect(html).not.toContain('Repair needed');
  });
  it('shows a repairable state when no leader is assigned', () => {
    const html = renderToStaticMarkup(<LeaderState group={group} />);
    expect(html).toContain('Repair needed');
  });
});
