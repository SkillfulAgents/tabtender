// Starts a private tabtenderd, runs the client conformance suite against it,
// and shuts it down. Usage: node run.mjs <path-to-tabtenderd>
//
// Honors TABTENDER_CHROME_PATH, TABTENDER_HEADLESS, and
// TABTENDER_E2E_CHROME_ARGS (extra browser flags, space separated).
import { spawn } from 'node:child_process';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import net from 'node:net';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const binary = process.argv[2];
if (!binary) {
  console.error('usage: node run.mjs <path-to-tabtenderd>');
  process.exit(2);
}

const freePort = () =>
  new Promise((resolve, reject) => {
    const server = net.createServer();
    server.once('error', reject);
    server.listen(0, '127.0.0.1', () => {
      const { port } = server.address();
      server.close(() => resolve(port));
    });
  });

const dir = mkdtempSync(path.join(process.platform === 'win32' ? os.tmpdir() : '/tmp', 'tt-clients-'));
const port = await freePort();
const apiKey = 'clients-key-0123456789abcdef0123456789abcdef';
const baseUrl = `http://127.0.0.1:${port}`;

const extraArgs = (process.env.TABTENDER_E2E_CHROME_ARGS ?? '').split(/\s+/).filter(Boolean);
const configPath = path.join(dir, 'config.yaml');
writeFileSync(configPath, extraArgs.length ? `browser:\n  extraArgs: ${JSON.stringify(extraArgs)}\n` : '');

const daemon = spawn(path.resolve(binary), [], {
  stdio: ['ignore', 'inherit', 'inherit'],
  env: {
    ...process.env,
    TABTENDER_CONFIG_PATH: configPath,
    TABTENDER_DATA_DIR: path.join(dir, 'data'),
    TABTENDER_MGMT_SOCKET: path.join(dir, 'mgmt.sock'),
    TABTENDER_SECRETS_BACKEND: 'file',
    TABTENDER_PORT: String(port),
    TABTENDER_API_KEY: apiKey,
  },
});
const daemonExit = new Promise((resolve) => daemon.once('exit', resolve));

async function waitForDaemon() {
  for (let attempt = 0; attempt < 300; attempt++) {
    if (daemon.exitCode !== null) throw new Error(`tabtenderd exited with code ${daemon.exitCode}`);
    try {
      const response = await fetch(`${baseUrl}/v1/health`, { headers: { Authorization: `Bearer ${apiKey}` } });
      if (response.ok) return;
    } catch {
      // Not listening yet.
    }
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  throw new Error('tabtenderd did not become ready');
}

let exitCode = 1;
try {
  await waitForDaemon();
  const suite = spawn(process.execPath, ['--test', '--test-concurrency=1', '--test-timeout=180000'], {
    cwd: path.dirname(fileURLToPath(import.meta.url)),
    stdio: 'inherit',
    env: { ...process.env, TABTENDER_URL: baseUrl, TABTENDER_API_KEY: apiKey },
  });
  exitCode = await new Promise((resolve) => suite.once('exit', (code) => resolve(code ?? 1)));
} catch (error) {
  console.error(error);
} finally {
  daemon.kill(process.platform === 'win32' ? undefined : 'SIGTERM');
  await daemonExit;
  rmSync(dir, { recursive: true, force: true, maxRetries: 20, retryDelay: 250 });
}
process.exit(exitCode);
