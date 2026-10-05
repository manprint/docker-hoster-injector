import { test, expect, Page } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { loadState, docker, dockerQuiet, run, PREFIX, startAgent, stopAgent, SEED } from './support';

interface Link { label: string; url: string; port: number; protocol: string }
interface Rec { address: string; names: string[]; container: string; container_id: string; state: string; side: string; links: Link[] }
interface Snap {
  records: Rec[];
  skipped: { container: string; state: string; reason: string }[];
  summary: { containers: number; records: number; names: number; skipped: number };
}

const state = loadState();

async function snapshot(base = state.base): Promise<Snap> {
  const r = await fetch(`${base}/api/entries`);
  expect(r.status).toBe(200);
  return r.json() as Promise<Snap>;
}

const WANT = ['pw-web', 'pw-internal', 'pw-multi', 'pw-db', 'pw-udp', 'pw-twonets', 'pw-compose_web_1', 'pw-clash-a', 'pw-clash-b'];

test.beforeAll(async () => {
  await expect.poll(async () => {
    const s = await snapshot();
    const have = new Set(s.records.map((r) => r.container));
    return WANT.every((n) => have.has(n));
  }, { timeout: 45_000, intervals: [300] }).toBe(true);
});

// Every page in these tests fails on a console error, a page error or a CSP
// violation, so a regression in any of them cannot pass unnoticed.
function watchErrors(page: Page): string[] {
  const problems: string[] = [];
  page.on('console', (m) => { if (m.type() === 'error' || m.type() === 'warning') problems.push(`console.${m.type()}: ${m.text()}`); });
  page.on('pageerror', (e) => problems.push(`pageerror: ${e.message}`));
  return problems;
}

test('the page shows what the API says, with no errors', async ({ page }) => {
  const problems = watchErrors(page);
  await page.goto(state.base);
  await expect(page.locator('#state')).toHaveText('live');

  const snap = await snapshot();
  await expect(page.locator('#published tbody tr')).toHaveCount(snap.records.length);
  await expect(page.locator('#skipped tbody tr')).toHaveCount(snap.skipped.length);

  const pills = await page.locator('.stat .pill').allTextContents();
  expect(pills[0]).toContain(`${snap.summary.containers} containers`);
  expect(pills[1]).toContain(`${snap.summary.records} records`);
  expect(pills[2]).toContain(`${snap.summary.names} names`);
  expect(pills[3]).toContain(`${snap.summary.skipped} skipped`);

  await expect(page).toHaveTitle('docker-hoster-injector');
  await expect(page.locator('footer')).toContainText('read-only');
  expect(problems).toEqual([]);
});

test('every row shows its container, state, address and names', async ({ page }) => {
  await page.goto(state.base);
  const snap = await snapshot();
  for (const r of snap.records) {
    const row = page.locator(`#published tbody tr[data-key="${r.container_id}|${r.address}"]`);
    await expect(row).toHaveCount(1);
    await expect(row.locator('td').nth(0)).toHaveText(r.container);
    await expect(row.locator('td').nth(1)).toHaveText(r.state);
    await expect(row.locator('td').nth(2)).toHaveText(r.address);
    await expect(row.locator('td').nth(3).locator('code.name')).toHaveText(r.names);
  }
});

test('the hosts file agrees with the page', async () => {
  const file = readFileSync(state.hosts, 'utf8');
  expect(file.startsWith(SEED)).toBe(true);
  // Only the managed block: the operator's own lines come first and may use
  // the same addresses (127.0.0.1 is both localhost and the published ports).
  const block = file.slice(file.indexOf('# BEGIN docker-hoster-injector'));
  expect(block).toContain('# END docker-hoster-injector');
  const snap = await snapshot();
  for (const r of snap.records) {
    const line = block.split('\n').find((l) => l.split(/\s+/)[0] === r.address);
    expect(line, `address ${r.address} is in the file`).toBeTruthy();
    for (const n of r.names) expect(line!.split(/\s+/)).toContain(n);
  }
});

test('links: web ports are anchors, other ports are plain text', async ({ page }) => {
  await page.goto(state.base);
  const snap = await snapshot();

  const anchors = await page.locator('td.links a.link').evaluateAll((els) =>
    els.map((a) => ({ href: (a as HTMLAnchorElement).href, target: (a as HTMLAnchorElement).target, rel: (a as HTMLAnchorElement).rel, text: a.textContent })));
  const expected = snap.records.flatMap((r) => r.links.filter((l) => l.url));
  expect(anchors.length).toBe(expected.length);
  expect(anchors.length).toBeGreaterThan(8);
  for (const a of anchors) {
    expect(a.href).toMatch(/^https?:\/\//);
    expect(a.target).toBe('_blank');
    expect(a.rel).toContain('noopener');
    expect(a.rel).toContain('noreferrer');
  }

  // The database and the UDP service are shown and are not anchors.
  const plain = await page.locator('td.links span.link').allTextContents();
  expect(plain.some((t) => t.endsWith(`:${state.ports.db}`))).toBe(true);
  expect(plain.some((t) => t.endsWith(`:${state.ports.udp}`))).toBe(true);
  const dbAnchors = anchors.filter((a) => a.text!.endsWith(`:${state.ports.db}`));
  expect(dbAnchors).toEqual([]);
});

test('clicking a link opens the container', async ({ page, context }) => {
  await page.goto(state.base);
  const link = page.locator(`td.links a.link[href^="http://pw-web.docker.local:${state.ports.web}"]`).first();
  await expect(link).toBeVisible();

  const [popup] = await Promise.all([context.waitForEvent('page'), link.click()]);
  // The test machine resolves the name through the hosts file of the agent
  // only if it is /etc/hosts, which this suite does not touch. The click must
  // still go where the link says.
  expect(popup.url()).toBe(`http://pw-web.docker.local:${state.ports.web}/`);
  await popup.close();

  // The container address is always reachable, whatever the resolver says.
  const direct = (await snapshot()).records
    .find((r) => r.container === 'pw-web' && r.side === 'container')!.links.find((l) => l.url)!;
  const resp = await page.request.get(direct.url);
  expect(resp.status()).toBe(200);
  expect((await resp.text()).trim()).toBe('ok');
});

test('every link the page offers answers', async ({ page }) => {
  const snap = await snapshot();
  const urls = new Set<string>();
  for (const r of snap.records) {
    // The names of this suite resolve through a private hosts file, which the
    // host's resolver does not read. Container addresses are reachable.
    if (r.side !== 'container') continue;
    for (const l of r.links) if (l.url && l.url.startsWith('http://')) urls.add(l.url);
  }
  expect(urls.size).toBeGreaterThan(4);
  for (const u of urls) {
    const resp = await page.request.get(u, { timeout: 10_000 });
    expect(resp.status(), u).toBe(200);
  }
});

test('the filter narrows the table and follows live updates', async ({ page }) => {
  await page.goto(state.base);
  const filter = page.getByLabel('Filter by container, name or address');
  const rows = page.locator('#published tbody tr');
  const all = await rows.count();

  await filter.fill('pw-twonets');
  const snap = await snapshot();
  await expect(rows).toHaveCount(snap.records.filter((r) => r.container === 'pw-twonets').length);

  await filter.fill(String(state.ports.db));
  await expect(rows).toHaveCount(snap.records.filter((r) => r.links.some((l) => l.label.endsWith(`:${state.ports.db}`))).length);

  await filter.fill('no-such-thing-anywhere');
  await expect(rows).toHaveCount(0);
  await expect(page.locator('#empty')).toBeVisible();

  await filter.fill('');
  await expect(rows).toHaveCount(all);
  await expect(page.locator('#empty')).toBeHidden();
});

test('the not-published table says why', async ({ page }) => {
  await page.goto(state.base);
  const text = await page.locator('#skipped tbody').innerText();
  expect(text).toContain('pw-hostnet');
  expect(text).toContain('shares the host network');
  expect(text).toContain('pw-nonet');
  expect(text).toContain('no network');
  // Two containers wanted "clash": one kept it, the other is told so.
  expect(text).toMatch(/clash\.docker\.local[\s\S]*conflict/);
  expect(text).toMatch(/name wanted by 2 containers/);
});

test('a container that starts appears without a reload, and leaves when it stops', async ({ page }) => {
  await page.goto(state.base);
  await expect(page.locator('#state')).toHaveText('live');
  await expect(page.locator('tr', { hasText: 'pw-live' })).toHaveCount(0);

  try {
    run('live', ['--expose', '8090'], 'busybox:latest', ['sh', '-c', 'echo ok > /tmp/index.html && exec httpd -f -p 8090 -h /tmp']);
    const row = page.locator('#published tbody tr', { hasText: 'pw-live' });
    await expect(row).toHaveCount(1, { timeout: 20_000 });
    await expect(row.locator('a.link')).toHaveText(/:8090$/);

    dockerQuiet('stop', '-t', '1', PREFIX + 'live');
    await expect(row).toHaveCount(0, { timeout: 20_000 });
  } finally {
    dockerQuiet('rm', '-f', PREFIX + 'live');
  }
  // The counters follow, too.
  const snap = await snapshot();
  await expect(page.locator('.stat .pill').nth(0)).toContainText(`${snap.summary.containers} containers`);
});

test('the filter is not reset by an update, and does not bring back old data', async ({ page }) => {
  await page.goto(state.base);
  const filter = page.getByLabel('Filter by container, name or address');
  await filter.fill('pw-live2');
  await expect(page.locator('#published tbody tr')).toHaveCount(0);

  try {
    run('live2', [], 'busybox:latest', ['sleep', '600']);
    await expect(page.locator('#published tbody tr')).toHaveCount(1, { timeout: 20_000 });
    expect(await filter.inputValue()).toBe('pw-live2');
  } finally {
    dockerQuiet('rm', '-f', PREFIX + 'live2');
  }
  await expect(page.locator('#published tbody tr')).toHaveCount(0, { timeout: 20_000 });
  // Re-typing the filter must not resurrect the container from a stale copy.
  await filter.fill('pw-live');
  await expect(page.locator('#published tbody tr')).toHaveCount(0);
});

test('a narrow screen does not scroll sideways', async ({ page }) => {
  await page.setViewportSize({ width: 380, height: 800 });
  await page.goto(state.base);
  await expect(page.locator('#published tbody tr').first()).toBeVisible();
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
  expect(overflow).toBeLessThanOrEqual(0);
});

test('on a wide screen no state or address is broken across lines', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(state.base);
  await expect(page.locator('#published tbody tr').first()).toBeVisible();
  const broken = await page.evaluate(() => {
    const out: string[] = [];
    for (const td of document.querySelectorAll('#published td:nth-child(2), #published td:nth-child(3)')) {
      const range = document.createRange();
      range.selectNodeContents(td);
      // One rectangle per line of text.
      if (range.getClientRects().length > 1) out.push(td.textContent ?? '');
    }
    return out;
  });
  expect(broken).toEqual([]);
});

test('on a narrow screen a row is a card and every value stays readable', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 800 });
  await page.goto(state.base);
  const first = page.locator('#published tbody tr').first();
  await expect(first).toBeVisible();
  // The column name is shown above the value.
  const labels = await first.locator('td').evaluateAll((tds) => tds.map((td) => (td as HTMLElement).dataset.label));
  expect(labels).toEqual(['Container', 'State', 'Address', 'Names', 'Open']);
  // The names are not squeezed into a sliver.
  const width = await first.locator('td').nth(3).evaluate((td) => td.getBoundingClientRect().width);
  expect(width).toBeGreaterThan(250);
  const box = await first.boundingBox();
  expect(box!.width).toBeLessThanOrEqual(390);
});

test('light and dark themes both render legibly', async ({ page }) => {
  const colours = async (scheme: 'light' | 'dark') => {
    await page.emulateMedia({ colorScheme: scheme });
    await page.goto(state.base);
    return page.evaluate(() => {
      const cs = getComputedStyle(document.body);
      return { bg: cs.backgroundColor, fg: cs.color };
    });
  };
  const light = await colours('light');
  const dark = await colours('dark');
  expect(light.bg).not.toBe(dark.bg);
  expect(light.fg).not.toBe(dark.fg);
});

test('the filter and the links can be used from the keyboard', async ({ page }) => {
  await page.goto(state.base);
  await page.keyboard.press('Tab');
  await expect(page.getByLabel('Filter by container, name or address')).toBeFocused();
  const link = page.locator('td.links a.link').first();
  await link.focus();
  await expect(link).toBeFocused();
});

test('read-only endpoints are exposed and writes are refused', async () => {
  for (const [path, type] of [['/healthz', 'json'], ['/metrics', 'text/plain'], ['/api/config', 'json']]) {
    const r = await fetch(state.base + path);
    expect(r.status).toBe(200);
    expect(r.headers.get('content-type')).toContain(type);
  }
  const r = await fetch(state.base + '/api/entries', { method: 'POST' });
  expect(r.status).toBe(405);
  const page = await fetch(state.base);
  expect(page.headers.get('content-security-policy')).toContain("default-src 'none'");
  expect(page.headers.get('x-content-type-options')).toBe('nosniff');
});

// --- polling mode and hostile data -----------------------------------------

test.describe('without server-sent events', () => {
  test('the page polls, shows new containers, and never turns a bad URL into a link', async ({ page }) => {
    const agent = await startAgent({ events: false });
    try {
      const problems: string[] = [];
      page.on('pageerror', (e) => problems.push(e.message));

      // The snapshot is tampered with on its way to the page: a link whose
      // URL is a script. It must be shown as text and never as an anchor.
      let tamper = true;
      await page.route('**/api/entries', async (route) => {
        const resp = await route.fetch();
        const body = await resp.json();
        if (tamper && body.records.length) {
          body.records[0].links.push({ label: 'evil:1', url: 'javascript:alert(document.domain)', port: 1, protocol: 'tcp' });
          body.records[0].links.push({ label: 'data:2', url: 'data:text/html,<script>alert(1)</script>', port: 2, protocol: 'tcp' });
        }
        await route.fulfill({ response: resp, json: body });
      });
      let dialogs = 0;
      page.on('dialog', async (d) => { dialogs++; await d.dismiss(); });

      await page.goto(agent.base);
      await expect(page.locator('#state')).toHaveText('polling', { timeout: 15_000 });
      await expect(page.locator('td.links span.link', { hasText: 'evil:1' })).toHaveCount(1);
      await expect(page.locator('td.links a[href^="javascript:"], td.links a[href^="data:"]')).toHaveCount(0);

      try {
        run('polled', [], 'busybox:latest', ['sleep', '600']);
        await expect(page.locator('#published tbody tr', { hasText: 'pw-polled' })).toHaveCount(1, { timeout: 25_000 });
      } finally {
        dockerQuiet('rm', '-f', PREFIX + 'polled');
      }
      await expect(page.locator('#published tbody tr', { hasText: 'pw-polled' })).toHaveCount(0, { timeout: 25_000 });

      expect(dialogs).toBe(0);
      expect(problems).toEqual([]);

      // The events endpoint is not served at all in this mode.
      const ev = await fetch(`${agent.base}/api/events`);
      expect(ev.status).toBe(404);
      tamper = false;
    } finally {
      expect(await stopAgent(agent)).toBe(0);
      // The agent leaves the file as it found it.
      expect(readFileSync(agent.hosts, 'utf8')).toBe(SEED);
    }
  });
});

test('a page recovers by itself when the agent comes back', async ({ page }) => {
  const agent = await startAgent();
  let next: Awaited<ReturnType<typeof startAgent>> | undefined;
  try {
    await page.goto(agent.base);
    await expect(page.locator('#state')).toHaveText('live');
    const rows = await page.locator('#published tbody tr').count();
    expect(rows).toBeGreaterThan(0);

    expect(await stopAgent(agent)).toBe(0);
    // The agent took its records with it: the file is the operator's again.
    expect(readFileSync(agent.hosts, 'utf8')).toBe(SEED);
    await expect(page.locator('#state')).toHaveText('reconnecting…', { timeout: 15_000 });

    next = await startAgent({ port: agent.port, hosts: agent.hosts });
    await expect(page.locator('#state')).toHaveText('live', { timeout: 30_000 });
    await expect(page.locator('#published tbody tr')).toHaveCount(rows, { timeout: 30_000 });
  } finally {
    if (next) {
      expect(await stopAgent(next)).toBe(0);
      expect(readFileSync(next.hosts, 'utf8')).toBe(SEED);
    }
    await stopAgent(agent);
  }
});
