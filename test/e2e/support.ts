import { execFileSync, spawn, ChildProcess } from 'node:child_process';
import { mkdtempSync, writeFileSync, readFileSync, existsSync, openSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import net from 'node:net';

export const REPO = resolve(__dirname, '../..');
export const AGENT = process.env.E2E_AGENT_BIN || join(REPO, 'bin/docker-hoster-injector');
export const STATE_FILE = join(tmpdir(), 'dhi-playwright-state.json');
export const SEED = '127.0.0.1\tlocalhost\n192.168.1.10\tnas.home\n';

export function docker(...args: string[]): string {
  return execFileSync('docker', args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });
}

export function dockerQuiet(...args: string[]): void {
  try { docker(...args); } catch { /* absent is fine */ }
}

export async function freePort(): Promise<number> {
  return new Promise((ok, fail) => {
    const s = net.createServer();
    s.listen(0, '127.0.0.1', () => {
      const { port } = s.address() as net.AddressInfo;
      s.close(() => ok(port));
    });
    s.on('error', fail);
  });
}

export interface AgentHandle {
  proc: ChildProcess;
  port: number;
  hosts: string;
  log: string;
  base: string;
}

export interface AgentOptions {
  events?: boolean;
  resync?: string;
  // port reuses a port, so that a restarted agent answers where the page looks.
  port?: number;
  // hosts reuses a hosts file.
  hosts?: string;
}

// startAgent runs the real binary over a private hosts file, so the suite
// never touches /etc/hosts.
export async function startAgent(opts: AgentOptions = {}): Promise<AgentHandle> {
  if (!existsSync(AGENT)) throw new Error(`agent binary not found at ${AGENT}: run "make build"`);
  const dir = mkdtempSync(join(tmpdir(), 'dhi-pw-'));
  const hosts = opts.hosts ?? join(dir, 'hosts');
  const log = join(dir, 'agent.log');
  if (!opts.hosts) writeFileSync(hosts, SEED);
  const port = opts.port ?? (await freePort());
  const fd = openSync(log, 'w');
  const proc = spawn(AGENT, [], {
    env: {
      ...process.env,
      HOSTS_FILE: hosts,
      HOSTS_MOUNT_MODE: 'file',
      DNS_SUFFIX: 'docker.local',
      TARGET_MODE: 'both',
      EVENT_DEBOUNCE: '100ms',
      RESYNC_INTERVAL: opts.resync ?? '2s',
      LOG_FORMAT: 'json',
      WEB_ENABLED: 'true',
      WEB_ADDR: `127.0.0.1:${port}`,
      WEB_EVENTS: opts.events === false ? 'false' : 'true',
    },
    stdio: ['ignore', fd, fd],
  });
  const base = `http://127.0.0.1:${port}`;
  const deadline = Date.now() + 30_000;
  for (;;) {
    if (proc.exitCode !== null) throw new Error(`the agent exited early:\n${readFileSync(log, 'utf8')}`);
    try {
      const r = await fetch(`${base}/healthz`);
      if (r.ok) break;
    } catch { /* not up yet */ }
    if (Date.now() > deadline) throw new Error(`the agent never became ready:\n${readFileSync(log, 'utf8')}`);
    await new Promise((r) => setTimeout(r, 150));
  }
  return { proc, port, hosts, log, base };
}

export async function stopAgent(a: AgentHandle): Promise<number | null> {
  if (a.proc.exitCode !== null) return a.proc.exitCode;
  const done = new Promise<number | null>((ok) => a.proc.once('exit', (code) => ok(code)));
  a.proc.kill('SIGTERM');
  const timer = setTimeout(() => a.proc.kill('SIGKILL'), 12_000);
  const code = await done;
  clearTimeout(timer);
  return code;
}

// The containers of the suite, all under one prefix so a failed run can be
// cleaned up by name.
export const PREFIX = 'pw-';
export const NETWORK = 'pw-net';

const HTTPD = (port: number) => ['sh', '-c', `echo ok > /tmp/index.html && exec httpd -f -p ${port} -h /tmp`];

export interface Provisioned {
  hostPorts: Record<string, number>;
}

export function removeAll(): void {
  const names = docker('ps', '-a', '--filter', `name=^${PREFIX}`, '--format', '{{.Names}}').split('\n').filter(Boolean);
  for (const n of names) dockerQuiet('rm', '-f', '-v', n);
  dockerQuiet('network', 'rm', NETWORK);
}

export function run(name: string, args: string[], image = 'busybox:latest', cmd: string[] = []): void {
  docker('run', '-d', '--name', PREFIX + name, ...args, image, ...cmd);
}

export function provision(ports: Record<string, number>): void {
  removeAll();
  docker('network', 'create', NETWORK);
  // Published on a host port, plain web server.
  run('web', ['-p', `${ports.web}:8080`], 'busybox:latest', HTTPD(8080));
  // Only reachable on the container's own address. A port is only known to
  // Docker, and so to the page, when it is declared: published or exposed.
  run('internal', ['--network', NETWORK, '--expose', '8081'], 'busybox:latest', HTTPD(8081));
  // Several published ports, one of them TLS by convention.
  run('multi', ['-p', `${ports.multiA}:8080`, '-p', `${ports.multiB}:8443`], 'busybox:latest',
    ['sh', '-c', 'echo ok > /tmp/index.html && (httpd -p 8443 -h /tmp &) ; exec httpd -f -p 8080 -h /tmp']);
  // Not web: a database port and a UDP port.
  run('db', ['-p', `${ports.db}:5432`], 'busybox:latest', ['nc', '-l', '-k', '-p', '5432']);
  run('udp', ['-p', `${ports.udp}:5353/udp`], 'busybox:latest', ['nc', '-l', '-u', '-p', '5353']);
  // On two networks.
  run('twonets', ['--network', NETWORK, '--expose', '8082'], 'busybox:latest', HTTPD(8082));
  docker('network', 'connect', 'bridge', PREFIX + 'twonets');
  // A compose-like name with an underscore, and a network alias.
  run('compose_web_1', ['--network', NETWORK, '--network-alias', 'frontend', '--expose', '8083'], 'busybox:latest', HTTPD(8083));
  // Two containers wanting the same alias.
  run('clash-a', ['--network', NETWORK, '--network-alias', 'clash', '--expose', '8084'], 'busybox:latest', HTTPD(8084));
  run('clash-b', ['--network', NETWORK, '--network-alias', 'clash', '--expose', '8084'], 'busybox:latest', HTTPD(8084));
  // Never published.
  run('hostnet', ['--network', 'host'], 'busybox:latest', HTTPD(18999));
  run('nonet', ['--network', 'none'], 'busybox:latest', ['sleep', '600']);
  run('stopped', [], 'busybox:latest', ['sleep', '600']);
  docker('stop', '-t', '1', PREFIX + 'stopped');
}

export function loadState(): { port: number; hosts: string; base: string; ports: Record<string, number> } {
  return JSON.parse(readFileSync(STATE_FILE, 'utf8'));
}
