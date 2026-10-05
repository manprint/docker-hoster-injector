import { writeFileSync } from 'node:fs';
import { freePort, provision, startAgent, STATE_FILE, docker } from './support';

export default async function globalSetup() {
  docker('version', '--format', '{{.Server.Version}}');
  const names = ['web', 'multiA', 'multiB', 'db', 'udp'];
  const ports: Record<string, number> = {};
  for (const n of names) ports[n] = await freePort();

  provision(ports);
  const agent = await startAgent();
  // The state is handed to the tests, and to the teardown, through a file:
  // the setup and the tests run in different processes.
  writeFileSync(STATE_FILE, JSON.stringify({
    port: agent.port, hosts: agent.hosts, base: agent.base, log: agent.log,
    pid: agent.proc.pid, ports,
  }));
}
