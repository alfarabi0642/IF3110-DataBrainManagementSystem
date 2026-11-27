import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

if (!process.env.DATABRAIN_TEST_ADMIN_URL) {
  console.error('DATABRAIN_TEST_ADMIN_URL wajib menunjuk disposable PostgreSQL fixture; lihat backend/dev/README.md.');
  process.exit(1);
}
const result = spawnSync('go', ['test', '-p', '1', '-count=1', '-timeout=5m', '-v', './internal/migration', '-run', '^TestBrowserSPA$'], {
  cwd: fileURLToPath(new URL('../../backend', import.meta.url)), stdio: 'inherit',
  env: { ...process.env, DATABRAIN_E2E: '1', DATABRAIN_NODE: process.execPath },
});
if (result.error) console.error(result.error.message);
process.exit(result.status ?? 1);
