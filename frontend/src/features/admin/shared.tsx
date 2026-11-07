import { useEffect, useRef, type FormEvent } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { apiFetch, ApiError, type Envelope } from '../../lib/api';
import type { Session } from '../../lib/session';

export type User = { id: string; username: string; display_name: string; is_active: boolean; is_platform_admin: boolean; revision: string };
export type Group = { id: string; name: string; is_active: boolean; revision: string };
export type UserDetail = User & { groups: Group[] };
export type Member = Pick<User, 'id' | 'username' | 'display_name' | 'is_active'>;
export type GroupDetail = Group & {
  members: Member[]; member_choices: Member[];
  grants: { database_id: string; database_name: string; access: string }[];
  database_choices: { id: string; name: string }[];
};
export type Connection = {
  id: string; name: string; host: string; port: number; database_name: string; username: string;
  ssl_mode: string; is_active: boolean; password_stored: boolean; config_revision: string; updated_at: string;
};
export type Probe = { ok: boolean; reason: string; server_version: number; latency_ms: number; latency: string };

export const status = (active: boolean) => active ? 'Active' : 'Inactive';
export const text = (form: HTMLFormElement | null, name: string) => String(new FormData(form ?? undefined).get(name) ?? '');
export const submit = (action: { isPending: boolean; mutate: () => void }) => (event: FormEvent<HTMLFormElement>) => {
  event.preventDefault();
  if (!action.isPending) action.mutate();
};

export function useAdminAction<T = { id: string }>(session: Session, path: string, body: () => unknown, done: (value: T) => void | Promise<void>) {
  const client = useQueryClient();
  return useMutation({
    meta: { access: true },
    mutationFn: () => apiFetch<T>(`/api${path}`, { method: 'POST', csrfToken: session.csrf_token, body: body() }),
    onError: async (error) => {
      if (error instanceof ApiError && error.status === 409) await client.invalidateQueries({ queryKey: ['admin'] });
    },
    onSuccess: async (result) => {
      const current = client.getQueryData<Envelope<Session>>(['session'])?.data;
      if (current?.state !== 'authenticated' || current.csrf_token !== session.csrf_token) return;
      await Promise.all([client.invalidateQueries({ queryKey: ['admin'] }), client.invalidateQueries({ queryKey: ['databases'] })]);
      await done(result.data);
    },
  });
}

export function FormError({ error, code = false }: { error: unknown; code?: boolean }) {
  const message = useRef<HTMLParagraphElement>(null);
  useEffect(() => { if (error) message.current?.focus(); }, [error]);
  if (!error) return null;
  return <p ref={message} tabIndex={-1} className="status failed" role="alert">{error instanceof Error ? error.message : 'Internal server error.'}{code && error instanceof ApiError && <> <code>{error.code}</code></>}</p>;
}

export function StatusField({ active }: { active: boolean }) {
  return <><label htmlFor="is_active">Status</label><select id="is_active" name="is_active" required defaultValue={String(active)}><option value="true">Active</option><option value="false">Inactive</option></select></>;
}

export function AccountType({ admin = false }: { admin?: boolean }) {
  return <><label htmlFor="is_platform_admin">Account type</label><select id="is_platform_admin" name="is_platform_admin" required defaultValue={String(admin)}><option value="false">Regular user</option><option value="true">Platform admin</option></select></>;
}

export function DisplayName({ value = '' }: { value?: string }) {
  return <><label htmlFor="display_name">Display name</label><input id="display_name" name="display_name" maxLength={80} autoComplete="off" required defaultValue={value} /></>;
}

export function SSLMode({ value = 'verify-full' }: { value?: string }) {
  return <><label htmlFor="ssl_mode">SSL mode</label><select id="ssl_mode" name="ssl_mode" defaultValue={value}><option value="verify-full">verify-full (verifies the server certificate)</option><option value="disable">disable (no encryption, development only)</option></select></>;
}

export function takePassword(form: HTMLFormElement | null) {
  const input = form?.elements.namedItem('password') as HTMLInputElement | null;
  const value = input?.value ?? '';
  // Password hanya hidup selama request, tanpa state atau mutation variables.
  if (input) input.value = '';
  return value;
}
