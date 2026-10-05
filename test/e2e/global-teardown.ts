import { readFileSync, existsSync, unlinkSync } from 'node:fs';
import { removeAll, STATE_FILE } from './support';

export default async function globalTeardown() {
  if (existsSync(STATE_FILE)) {
    const { pid } = JSON.parse(readFileSync(STATE_FILE, 'utf8'));
    try { process.kill(pid, 'SIGTERM'); } catch { /* already gone */ }
    // Give it the time it needs to hand the hosts file back.
    await new Promise((r) => setTimeout(r, 1500));
    unlinkSync(STATE_FILE);
  }
  removeAll();
}
