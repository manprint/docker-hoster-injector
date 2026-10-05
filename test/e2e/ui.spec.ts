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

// The page lists one row per container.
function containersOf(snap: Snap): string[] {
  return [...new Set(snap.records.map((r) => r.container_id))];
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
  // One row per container, however many addresses it has.
  await expect(page.locator('#published tbody tr')).toHaveCount(containersOf(snap).length);
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

test('every row shows its container, state, addresses and names', async ({ page }) => {
  await page.goto(state.base);
  const snap = await snapshot();
  for (const id of containersOf(snap)) {
    const recs = snap.records.filter((r) => r.container_id === id);
    const row = page.locator(`#published tbody tr[data-key="${id}"]`);
    await expect(row).toHaveCount(1);
    await expect(row.locator('td').nth(0)).toHaveText(recs[0].container);
    await expect(row.locator('td').nth(1)).toHaveText(recs[0].state);
    // The addresses, one per line, in the order the resolver tries them.
    await expect(row.locator('td.addr div')).toHaveText(recs.map((r) => r.address));
    await expect(row.locator('td').nth(3).locator('code.name')).toHaveText(recs[0].names);
  }
});

test('IPv6 addresses are not listed', async ({ page }) => {
  await page.goto(state.base);
  await expect(page.locator('#published tbody tr').first()).toBeVisible();
  const addrs = await page.locator('#published td.addr div').allTextContents();
  expect(addrs.length).toBeGreaterThan(5);
  expect(addrs.filter((a) => a.includes(':'))).toEqual([]);
  // The file keeps them: the agent still publishes ::1 for a published port.
  expect(readFileSync(state.hosts, 'utf8')).toContain('::1');
});

test('the hosts file agrees with the page', async () => {
  const file = readFileSync(state.hosts, 'utf8');
  expect(file.startsWith(SEED)).toBe(true);
  // Only the managed block: the operator's own lines come first and may use
  // the same addresses (127.0.0.1 is both localhost and the published ports).
  const block = file.slice(file.indexOf('# BEGIN docker-hoster-injector'));
  expect(block).toContain('# END docker-hoster-injector');
  const snap = await snapshot();
  expect(snap.records.length).toBeGreaterThan(5);
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
  const plain = await page.locator('td.links span.link .dest').allTextContents();
  expect(plain.some((t) => t.endsWith(`:${state.ports.db}`))).toBe(true);
  expect(plain.some((t) => t.endsWith(`:${state.ports.udp}`))).toBe(true);
  const dbAnchors = anchors.filter((a) => a.text!.endsWith(`:${state.ports.db}`));
  expect(dbAnchors).toEqual([]);
});

test('every port says whether it is TCP or UDP', async ({ page }) => {
  await page.goto(state.base);
  const snap = await snapshot();
  const chips = await page.locator('td.links .link').evaluateAll((els) => els.map((e) => ({
    dest: e.querySelector('.dest')!.textContent,
    proto: e.querySelector('.proto')!.textContent,
    attr: (e as HTMLElement).dataset.proto,
    color: getComputedStyle(e.querySelector('.proto')!).color,
    border: getComputedStyle(e).borderStyle,
  })));
  const expected = snap.records.flatMap((r) => r.links);
  expect(chips.length).toBe(expected.length);
  for (const c of chips) {
    expect(['TCP', 'UDP']).toContain(c.proto);
    expect(c.attr).toBe(c.proto!.toLowerCase());
  }
  // The UDP service of the suite is marked, and looks different from TCP.
  const udp = chips.filter((c) => c.dest!.endsWith(`:${state.ports.udp}`));
  expect(udp.length).toBeGreaterThan(0);
  for (const c of udp) { expect(c.proto).toBe('UDP'); expect(c.border).toBe('dashed'); }
  const tcp = chips.filter((c) => c.dest!.endsWith(`:${state.ports.db}`));
  expect(tcp.length).toBeGreaterThan(0);
  for (const c of tcp) { expect(c.proto).toBe('TCP'); expect(c.border).toBe('solid'); }
  expect(new Set(chips.map((c) => c.color)).size).toBeGreaterThan(1);

  // The filter understands the protocol.
  await page.getByLabel('Filter by container, name or address').fill('udp');
  await expect(page.locator('#published tbody tr')).toHaveCount(
    new Set(snap.records.filter((r) => r.links.some((l) => l.protocol === 'udp')).map((r) => r.container_id)).size);
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
    .find((r) => r.container === 'pw-internal' && r.side === 'container')!.links.find((l) => l.url)!;
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

test('the filter narrows the table', async ({ page }) => {
  await page.goto(state.base);
  const filter = page.getByLabel('Filter by container, name or address');
  const rows = page.locator('#published tbody tr');
  const all = await rows.count();
  const snap = await snapshot();

  await filter.fill('pw-twonets');
  await expect(rows).toHaveCount(1);

  // By the port of a link.
  await filter.fill(String(state.ports.db));
  await expect(rows).toHaveCount(
    containersOf(snap).filter((id) => snap.records.some((r) => r.container_id === id && r.links.some((l) => l.label.endsWith(`:${state.ports.db}`)))).length);

  // By an address.
  const addr = snap.records.find((r) => r.container === 'pw-web' && r.side === 'container')!.address;
  await filter.fill(addr);
  await expect(rows).toHaveCount(1);

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
    await expect(row.locator('a.link .dest')).toHaveText(/:8090$/);

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

test('on a wide screen the columns are sized for their content', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(state.base);
  await expect(page.locator('#published tbody tr').first()).toBeVisible();

  // Nothing short is broken across lines: states, addresses, link chips.
  const broken = await page.evaluate(() => {
    const out: string[] = [];
    const lines = (el: Element) => { const r = document.createRange(); r.selectNodeContents(el); return r.getClientRects().length; };
    for (const el of document.querySelectorAll('#published td.state, #published td.addr div, #published .link .dest, #published .link .proto')) {
      if (lines(el) > 1) out.push(el.textContent ?? '');
    }
    return out;
  });
  expect(broken).toEqual([]);

  // Each column has room for what it holds.
  const widths = await page.locator('#published thead th').evaluateAll((ths) => ths.map((t) => t.getBoundingClientRect().width));
  const [container, st, address, names, open] = widths;
  expect(container).toBeGreaterThan(150);
  expect(st).toBeGreaterThan(90);
  expect(address).toBeGreaterThan(130);
  expect(names).toBeGreaterThan(300);
  expect(open).toBeGreaterThan(250);

  // The page is not wider than the screen.
  expect(await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)).toBeLessThanOrEqual(0);
});

test('the columns do not move when the content changes', async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(state.base);
  await expect(page.locator('#published tbody tr').first()).toBeVisible();
  const measure = () => page.locator('#published thead th').evaluateAll((ths) => ths.map((t) => Math.round(t.getBoundingClientRect().width)));
  const before = await measure();

  try {
    // A container with a very long name and a long list of ports.
    run('x'.repeat(40) + '-long-name-for-the-layout', ['--expose', '8000', '--expose', '8001', '--expose', '8002'], 'busybox:latest', ['sleep', '600']);
    await expect(page.locator('#published tbody tr', { hasText: 'long-name-for-the-layout' })).toHaveCount(1, { timeout: 20_000 });
    expect(await measure()).toEqual(before);
    expect(await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)).toBeLessThanOrEqual(0);
  } finally {
    dockerQuiet('rm', '-f', PREFIX + 'x'.repeat(40) + '-long-name-for-the-layout');
    // The removal reaches the page as one more update. Wait for it, or it
    // lands in the middle of the next test and redraws the table under it.
    await expect.poll(async () => (await snapshot()).records.some((r) => r.container.includes('long-name-for-the-layout')),
      { timeout: 20_000 }).toBe(false);
  }
});

test('on a narrow screen a row is a card and every value stays readable', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 800 });
  await page.goto(state.base);
  const first = page.locator('#published tbody tr').first();
  await expect(first).toBeVisible();

  // The head of the card: name and state on the same line.
  const head = await first.evaluate((tr) => {
    const c = tr.querySelector('td.container')!.getBoundingClientRect();
    const s = tr.querySelector('td.state')!.getBoundingClientRect();
    return { cTop: c.top, cBottom: c.bottom, cLeft: c.left, sTop: s.top, sBottom: s.bottom, sLeft: s.left };
  });
  // The state sits beside the name, within its height, and to its right.
  expect(head.sTop).toBeGreaterThanOrEqual(head.cTop - 1);
  expect(head.sBottom).toBeLessThanOrEqual(head.cBottom + 1);
  expect(head.sLeft).toBeGreaterThan(head.cLeft);

  // The other values are labelled with their column name.
  const labels = await first.locator('td:not(.container):not(.state)').evaluateAll((tds) =>
    tds.filter((td) => (td as HTMLElement).offsetParent !== null).map((td) => (td as HTMLElement).dataset.label));
  expect(labels).toEqual(expect.arrayContaining(['Names']));
  expect(labels.every((l) => ['Address', 'Addresses', 'Names', 'Open'].includes(l!))).toBe(true);

  // The names are not squeezed into a sliver, and the card fits the screen.
  const width = await first.locator('td').nth(3).evaluate((td) => td.getBoundingClientRect().width);
  expect(width).toBeGreaterThan(250);
  const box = await first.boundingBox();
  expect(box!.width).toBeLessThanOrEqual(390);
});

test('on a narrow screen a container with nothing to open has no empty heading', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 800 });
  await page.goto(state.base);
  await expect(page.locator('#published tbody tr').first()).toBeVisible();
  // pw-db and pw-udp have only non-web ports, so their Open cell holds text;
  // a published container with no ports at all has none. The empty ones must
  // not be shown.
  const emptyShown = await page.locator('#published td.links:empty').evaluateAll((tds) =>
    tds.filter((td) => (td as HTMLElement).offsetParent !== null).length);
  expect(emptyShown).toBe(0);
});

for (const width of [320, 360, 414, 768]) {
  test(`at ${width}px nothing overflows and the chips are easy to tap`, async ({ page }) => {
    await page.setViewportSize({ width, height: 800 });
    await page.goto(state.base);
    await expect(page.locator('#published tbody tr').first()).toBeVisible();
    expect(await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)).toBeLessThanOrEqual(0);

    const heights = await page.locator('#published a.link').evaluateAll((els) => els.map((e) => e.getBoundingClientRect().height));
    expect(heights.length).toBeGreaterThan(5);
    for (const h of heights) expect(h).toBeGreaterThanOrEqual(30);

    // A chip never runs off the card that holds it.
    const out = await page.evaluate(() => {
      const bad: string[] = [];
      for (const a of document.querySelectorAll('#published a.link')) {
        const r = a.getBoundingClientRect();
        const card = a.closest('tr')!.getBoundingClientRect();
        if (r.right > card.right + 1 || r.left < card.left - 1) bad.push(a.textContent ?? '');
      }
      return bad;
    });
    expect(out).toEqual([]);
  });
}

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
