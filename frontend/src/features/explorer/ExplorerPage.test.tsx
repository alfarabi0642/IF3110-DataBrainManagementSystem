import { describe, expect, it } from 'vitest';
import { renderToStaticMarkup } from 'react-dom/server';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { MemoryRouter, Route, Routes } from 'react-router';
import { Pager } from '../../app/ui';
import type { Session } from '../../lib/session';
import { ExplorerPage } from './ExplorerPage';

describe('explorer', () => {
  it('retains quoted identifiers and the selected tab in pagination links', () => {
    const params = new URLSearchParams({ schema: 'Odd &"猫.schema', table: "Order '<x>.table", tab: 'data', limit: '2', offset: '2' });
    const html = renderToStaticMarkup(<MemoryRouter initialEntries={[`/databases/first/explorer?${params}`]}>
      <Pager page={{ limit: 2, offset: 2, has_more: true, prev_offset: 0, next_offset: 4 }} />
    </MemoryRouter>);
    const links = [...html.matchAll(/href="([^"]+)"/g)].map(match => new URL(match[1].replaceAll('&amp;', '&'), 'http://localhost'));
    expect(links).toHaveLength(2);
    for (const link of links) {
      expect(link.pathname).toBe('/databases/first/explorer');
      for (const key of ['schema', 'table', 'tab', 'limit']) expect(link.searchParams.get(key)).toBe(params.get(key));
    }
    expect(links.map(link => link.searchParams.get('offset'))).toEqual(['0', '4']);
  });

  it('renders SQL NULL, empty text, literal null and exact numeric text distinctly', () => {
    const session: Session = { state: 'authenticated', csrf_token: 'fixture', absolute_expires_at: '',
      user: { id: 'viewer', username: 'viewer', display_name: 'Viewer', is_platform_admin: false } };
    const search = '?schema=public&table=items&limit=2';
    const client = new QueryClient();
    client.setQueryData(['explorer', 'viewer', 'first', search], { request_id: 'fixture', data: {
      database: { id: 'first', name: 'First', access: 'view_only' }, schema: 'public', table: 'items', tab: 'data',
      schemas: [], tables: [], columns: [], constraints: [], indexes: [],
      data: { columns: ['optional', 'empty', 'literal', 'big', 'numeric'], ordering: 'primary_key_asc', rows: [[
        { null: true, text: '' }, { null: false, text: '' }, { null: false, text: 'null' },
        { null: false, text: '9223372036854775807' }, { null: false, text: '12345678901234567890.1234567890' },
      ]] },
    } });
    const html = renderToStaticMarkup(<QueryClientProvider client={client}><MemoryRouter initialEntries={['/databases/first/explorer' + search]}>
      <Routes><Route path="/databases/:id/explorer" element={<ExplorerPage session={session} />} /></Routes>
    </MemoryRouter></QueryClientProvider>);
    expect(html).toContain('<span class="null">NULL</span>');
    expect(html).toContain('<span class="empty-string">(empty)</span>');
    expect(html).toContain('<td class="cell">null</td>');
    expect(html).toContain('<td class="cell">9223372036854775807</td>');
    expect(html).toContain('<td class="cell">12345678901234567890.1234567890</td>');
    expect(html).not.toContain('/drafts');
    client.clear();
  });
});
