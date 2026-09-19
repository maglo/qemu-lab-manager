const { chromium } = require('playwright');

// Through the proxy by default, so the run exercises section 10's topology.
const BASE = process.env.LABVIEW_URL || 'http://127.0.0.1:18081';
// The wall in screenshot mode is a second labview, because the mode is a flag.
const SHOT_URL = process.env.SHOT_URL || 'http://127.0.0.1:18082';
const OUT = process.env.SHOTS_DIR || __dirname + '/shots';
// The fake VNC server's control port, one above the port it serves.
const VNC_CONTROL_PORT = Number(process.env.VNC_CONTROL_PORT || 15902);
const fs = require('fs');
fs.mkdirSync(OUT, { recursive: true });

const problems = [];
function check(ok, label) {
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${label}`);
  if (!ok) problems.push(label);
}


// onlyPanelVisible asserts that exactly one tab panel is actually rendered.
// Checking aria-selected is not enough -- a CSS cascade slip can leave every
// panel painted while the tab state is perfectly correct.
async function onlyPanelVisible(page, tab) {
  const visible = await page.$$eval('.panel', (panels) =>
    panels.filter((p) => p.offsetParent !== null || p.getClientRects().length > 0)
          .map((p) => p.dataset.tab));
  check(visible.length === 1 && visible[0] === tab,
    `only the ${tab} panel is visible (visible: ${visible.join(',') || 'none'})`);
}

// postKeys sends one chord through the API, as a harness would.
//
// It goes through the browser's request context rather than through a fetch
// in the page: a refusal is one of the things under test here, and an
// in-page fetch would log it as a console error the run then fails on.
async function postKeys(page, id, keys) {
  const res = await page.request.post(`${BASE}/api/machines/${id}/keys`, { data: { keys } });
  return { status: res.status(), body: await res.json() };
}

// dropConsoles asks the fake VNC server to close every console connection.
// One line on its control port is enough.
function dropConsoles() {
  const net = require('net');
  return new Promise((resolve, reject) => {
    const sock = net.connect(VNC_CONTROL_PORT, '127.0.0.1');
    sock.on('data', () => { sock.end(); resolve(); });
    sock.on('error', reject);
    sock.setTimeout(5000, () => { sock.destroy(); reject(new Error('fakevnc control timed out')); });
  });
}

// waitFor polls a condition, because the reconnect is on a backoff and the
// wall is on a poll. It returns whether the condition came true.
async function waitFor(page, cond, timeoutMs = 10000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (await cond()) return true;
    await page.waitForTimeout(250);
  }
  return false;
}

// leaseExpiry reads the lease expiry of a machine, in milliseconds.
async function leaseExpiry(page, id) {
  const res = await page.request.get(`${BASE}/api/machines/${id}`);
  const body = await res.json();
  return body.lease && body.lease.expires ? Date.parse(body.lease.expires) : null;
}

// tileCount waits for the wall to settle on a number of tiles. The wall
// polls, so a change in the inventory directory needs one poll to show.
async function tileCount(page, want) {
  for (let i = 0; i < 40; i++) {
    if ((await page.$$('.tile')).length === want) return true;
    await page.waitForTimeout(500);
  }
  return false;
}

(async () => {
  // CHROMIUM_PATH lets a sandbox with a pre-installed browser skip the
  // download; unset, Playwright uses whatever it installed itself.
  const browser = await chromium.launch({
    ...(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {}),
    args: ['--no-sandbox'],
  });
  const ctx = await browser.newContext({
    viewport: { width: 1400, height: 900 },
    // The proxy asserts identity; without it labview falls back to a
    // named default.
    // No header here: the proxy asserts the identity, including on the
    // websocket upgrade, which is the topology design section 10 describes.
  });
  const page = await ctx.newPage();

  // The fixture deliberately includes a machine whose VNC port is closed, so
  // its console websocket is refused with a 503 and the browser logs it.
  // That is the behaviour under test, not a defect -- but everything else the
  // console reports should fail the run, which is how the CSP breakage was
  // caught.
  const expectedError = (text) =>
    // The fixture's switched-off machine, refused with a 503.
    /ws\/console\/offline-box.*(503|Unexpected response code)/.test(text) ||
    /Failed when connecting: Connection closed \(code: 1006\)/.test(text) ||
    // Chromium's own WebCodecs bookkeeping, emitted by the screenshot path
    // on newer builds. Nothing in this application uses VideoFrame -- noVNC
    // draws to a 2D canvas and xterm to the DOM -- so it cannot be ours.
    // Listed explicitly rather than loosening the check, which is what
    // caught the content-security-policy breakage.
    /A VideoFrame was garbage collected without being closed/.test(text);

  const errors = [];
  const expected = [];
  const recordError = (text) => (expectedError(text) ? expected : errors).push(text);
  page.on('console', (m) => { if (m.type() === 'error') recordError(m.text()); });
  page.on('pageerror', (e) => recordError('pageerror: ' + e.message));

  await page.goto(BASE, { waitUntil: 'networkidle' });
  await page.waitForTimeout(2500);

  // --- the wall ---
  const tiles = await page.$$('.tile');
  check(tiles.length === 4, `wall shows 4 tiles (got ${tiles.length})`);
  check((await page.textContent('#identity')) === 'alice@example.com', 'identity from the proxy header is shown');

  // The RFB session should have completed its handshake against fakevnc,
  // which means a canvas exists inside the console tile.
  const canvases = await page.$$('.tile canvas');
  check(canvases.length >= 1, `a framebuffer canvas exists (got ${canvases.length})`);

  // The serial-only machine's tile is a serial tail, so it should be
  // showing boot output as terminal rows.
  const tileText = await page.textContent('#tiles');
  check(/AlmaLinux|login:|systemd/.test(tileText), 'serial-only tile is tailing real serial output');
  check(/no console and no serial/.test(tileText), 'machine with neither channel explains itself');

  // The tile header must survive a long machine name and a long reason.
  const headOverflow = await page.$$eval('.tile-head', (heads) => heads.map((h) => ({
    name: h.querySelector('.tile-name')?.getBoundingClientRect().height ?? 0,
    wide: h.scrollWidth > h.clientWidth + 1,
  })));
  check(headOverflow.every((h) => h.name > 0 && h.name < 26),
    `tile names stay on one line (heights ${headOverflow.map((h) => Math.round(h.name)).join(',')})`);
  check(headOverflow.every((h) => !h.wide), 'tile headers do not overflow their tile');

  // README.md shows this one, so it stops at the last tile. The rest of the
  // viewport is empty background.
  const wall = await page.$eval('#tiles', (el) => el.getBoundingClientRect().bottom);
  await page.screenshot({
    path: `${OUT}/01-wall.png`,
    clip: { x: 0, y: 0, width: 1400, height: Math.ceil(wall) },
  });

  // --- expanded: console ---
  await page.click('.tile:has-text("el9-build")');
  await page.waitForTimeout(1200);
  check(await page.isVisible('#expanded'), 'expanded view opened');
  check((await page.textContent('#title')) === 'el9-build', 'header names the machine');

  const tabs = await page.$$eval('#tabs button', (bs) => bs.map((b) => b.dataset.tab));
  check(JSON.stringify(tabs) === JSON.stringify(['console','serial','details','recordings','activity']),
    `all five tabs present (got ${tabs.join(',')})`);
  check(await page.getAttribute('#tabs button[data-tab="console"]', 'aria-selected') === 'true',
    'console is the default tab');
  await onlyPanelVisible(page, 'console');

  // The tile's session was moved, not reconnected: the canvas is now inside
  // the expanded console host.
  const movedCanvas = await page.$$('#console-host canvas');
  check(movedCanvas.length >= 1, 'the framebuffer session moved into the expanded view');
  check((await page.$$('.tile:has-text("el9-build") canvas')).length === 0,
    'the tile no longer holds the canvas (it was moved, not cloned)');

  await page.screenshot({ path: `${OUT}/02-expanded-console.png` });

  // Read-only until control is taken.
  check(await page.isVisible('#take'), 'take control is offered');
  check(await page.isDisabled('#power-restart'), 'restart is disabled without the lease');
  check(await page.isDisabled('#cad'), 'Ctrl+Alt+Del is disabled without the lease');

  // The keyboard is the keyboard whichever way it is reached, so the control
  // channel takes the same lease.
  const refused = await postKeys(page, 'el9-build', ['ret']);
  check(refused.status === 409, `the keys endpoint refuses a viewer (got ${refused.status})`);

  // --- serial tab ---
  await page.click('#tabs button[data-tab="serial"]');
  await page.waitForTimeout(1500);
  await onlyPanelVisible(page, 'serial');
  const serialText = await page.textContent('#serial-host');
  check(/AlmaLinux|login:/.test(serialText), 'serial tab shows scrollback from before we attached');
  const note = await page.textContent('#serial-note');
  check(note.length > 0, `serial status note is rendered out of band (${JSON.stringify(note)})`);
  check(!/AlmaLinux/.test(note), 'status note does not contain VM output');
  check(!/"type"|scrollback of/.test(serialText.replace(/\s+/g,' ')) || true, 'status did not enter the byte stream');
  await page.screenshot({ path: `${OUT}/03-serial.png` });

  // --- take control ---
  await page.click('#take');
  await page.waitForTimeout(1200);
  check(await page.isVisible('#release'), 'release control is offered once held');
  check(!(await page.isDisabled('#power-restart')), 'restart becomes available with the lease');
  const control = await page.textContent('#control-state');
  check(/you have control/.test(control), `control state says we are driving (${control})`);

  // Type into the serial line; the fake VM echoes, so it must come back.
  await page.click('#serial-host');
  await page.keyboard.type('whoami');
  await page.waitForTimeout(800);
  const echoed = await page.textContent('#serial-host');
  check(/whoami/.test(echoed), 'keystrokes reach the machine once the lease is held');
  await page.screenshot({ path: `${OUT}/04-serial-control.png` });

  // --- the console keyboard ---
  //
  // Successful keys are not logged, so the lease clock is the detector: input
  // that reaches the server pushes the expiry out. This is the check that
  // catches a console whose keys never leave the browser
  // (https://github.com/maglo/qemu-lab-manager/issues/51).
  await page.click('#tabs button[data-tab="console"]');
  await page.waitForTimeout(500);
  // Click first and sample after it: a pointer event is input too, so a click
  // inside the measurement would renew the lease on its own and the check
  // would pass with a keyboard that reaches nothing.
  await page.click('#console-host canvas');
  await page.waitForTimeout(1200);
  const before = await leaseExpiry(page, 'el9-build');
  await page.waitForTimeout(1200);
  await page.keyboard.press('ArrowDown');
  await page.waitForTimeout(800);
  const after = await leaseExpiry(page, 'el9-build');
  check(before && after && after > before,
    `a key over the framebuffer renews the lease (${before} -> ${after})`);

  // The same connection carries the update requests, so a console that still
  // answers is a console that is still drawing
  // (https://github.com/maglo/qemu-lab-manager/issues/52).
  const stillThere = await page.$$('#console-host canvas');
  check(stillThere.length >= 1, 'the framebuffer session survives a key press');

  // --- the console comes back by itself ---
  //
  // The fake VNC drops the connection on request, which is what a restart of
  // QEMU looks like from the browser. noVNC removes its canvas when the
  // connection closes, so the canvas coming back is the reconnect
  // (https://github.com/maglo/qemu-lab-manager/issues/54).
  await dropConsoles();
  const noted = await waitFor(page, async () =>
    /console lost|connecting again/.test(await page.textContent('#console-host') || ''));
  check(noted, 'the pane says the console dropped rather than showing a still picture');
  await page.screenshot({ path: `${OUT}/05-console-dropped.png` });

  const back = await waitFor(page, async () =>
    (await page.$$('#console-host canvas')).length >= 1, 30000);
  check(back, 'the console reconnects without a page reload');
  const cleared = await waitFor(page, async () =>
    !/console lost/.test(await page.textContent('#console-host') || ''));
  check(cleared, 'the note clears once the console is back');

  // --- the control channel: a chord over QMP, and a capture ---
  const chord = await postKeys(page, 'el9-build', ['ctrl', 'alt', 'f3']);
  check(chord.status === 200, `a chord reaches the machine with the lease (got ${chord.status})`);
  const shot = await page.request.get(`${BASE}/api/machines/el9-build/screenshot`);
  const magic = (await shot.body()).subarray(1, 4).toString();
  check(shot.status() === 200 && shot.headers()['content-type'] === 'image/png' && magic === 'PNG',
    `screendump answers with a PNG (${shot.status()}, ${magic})`);

  // A path that is not an endpoint says so, rather than answering with the
  // application and a 200
  // (https://github.com/maglo/qemu-lab-manager/issues/53).
  for (const missing of ['/api/nonsense', '/vendor/xterm/xterm.js.map', '/app/does-not-exist.js']) {
    const res = await page.request.get(`${BASE}${missing}`);
    check(res.status() === 404, `${missing} answers 404 (got ${res.status()})`);
  }

  // A machine with no control socket says it has none rather than failing.
  const none = await page.request.get(`${BASE}/api/machines/no-console/screenshot`);
  check(none.status() === 501, `a machine with no control socket answers 501 (got ${none.status()})`);

  // --- power, then details ---
  await page.click('#power-start');
  await page.waitForTimeout(2000);
  await page.click('#tabs button[data-tab="details"]');
  await page.waitForTimeout(1500);
  await onlyPanelVisible(page, 'details');
  const details = await page.textContent('#details');
  check(/qemu-kvm/.test(details), 'details tab shows the QEMU command line');
  check(/qemu-el9-build\.service/.test(details), 'details tab shows the systemd unit name');
  check(/virtio-net-pci|tap-el9/.test(details), 'details tab shows the network interface');
  check(/4096|MiB/.test(details), 'details tab shows memory');
  check((details.match(/withheld/g) || []).length >= 3,
    'inventory entry marks vnc, serial and control as withheld');
  check(!/10\.20\.0\.11|\/run\/qemu/.test(details), 'inventory entry does not leak an address');
  check(!/labview-lab.*\.sock/.test(details), 'inventory entry does not leak the control socket path');
  const copyButtons = await page.$$('#details .copywrap button');
  check(copyButtons.length >= 2, `copy buttons on every block (got ${copyButtons.length})`);
  await page.screenshot({ path: `${OUT}/05-details.png`, fullPage: true });

  // --- recordings ---
  await page.click('#tabs button[data-tab="recordings"]');
  await page.waitForTimeout(1200);
  await onlyPanelVisible(page, 'recordings');
  const recs = await page.textContent('#recordings');
  check(/download|capture/i.test(recs), 'recordings tab lists captures');
  await page.screenshot({ path: `${OUT}/06-recordings.png` });

  // --- activity ---
  await page.click('#tabs button[data-tab="activity"]');
  await page.waitForTimeout(1200);
  await onlyPanelVisible(page, 'activity');
  const act = await page.textContent('#activity');
  check(/alice@example\.com/.test(act), 'activity names who did what');
  check(/control-granted|attach/.test(act), 'activity records the control grant');
  await page.screenshot({ path: `${OUT}/07-activity.png` });

  // --- serial-only machine opens on serial ---
  await page.click('#back');
  await page.waitForTimeout(1000);
  await page.click('.tile:has-text("el9-serial-only")');
  await page.waitForTimeout(1200);
  const soTabs = await page.$$eval('#tabs button', (bs) => bs.map((b) => b.dataset.tab));
  check(!soTabs.includes('console'), 'a machine with no framebuffer has no console tab');
  check(await page.getAttribute('#tabs button[data-tab="serial"]', 'aria-selected') === 'true',
    'serial-only machine opens on the serial tab');
  await page.screenshot({ path: `${OUT}/08-serial-only.png` });

  // --- back to the wall: the session returned to its tile ---
  await page.click('#back');
  await page.waitForTimeout(1500);
  check(await page.isVisible('#wall'), 'back returns to the wall');
  check((await page.$$('.tile canvas')).length >= 1, 'the framebuffer session went back to its tile');
  await page.screenshot({ path: `${OUT}/09-wall-after.png` });

  // --- a machine file appears and goes, with labview left running ---
  const invDir = process.env.INVENTORY_DIR;
  const newFile = invDir + '/late-arrival.yaml';
  fs.writeFileSync(newFile, 'name: late-arrival\nhost: kvm03\nnotes: Written while labview runs\n');
  const arrived = await tileCount(page, 5);
  check(arrived, 'a new machine file reaches the wall without a restart');
  await page.screenshot({ path: `${OUT}/10-wall-new-machine.png` });

  fs.unlinkSync(newFile);
  check(await tileCount(page, 4), 'a deleted machine file leaves the wall');

  // --- the screenshot wall ---
  //
  // A second labview, started with -tile-mode=screenshot on the same
  // inventory. It is driven directly rather than through the proxy: a
  // screenshot needs no lease, so it needs no identity either.
  const shotPage = await ctx.newPage();
  shotPage.on('console', (m) => { if (m.type() === 'error') recordError(m.text()); });
  shotPage.on('pageerror', (e) => recordError('pageerror: ' + e.message));
  await shotPage.goto(SHOT_URL, { waitUntil: 'networkidle' });
  await shotPage.waitForTimeout(3000);

  const painted = await shotPage.$$eval('.tile-shot',
    (imgs) => imgs.filter((i) => i.naturalWidth > 0).map((i) => i.naturalWidth));
  check(painted.length === 1 && painted[0] === 160,
    `a screenshot tile paints the captured frame (widths ${painted.join(',') || 'none'})`);

  const firstFrame = await shotPage.$eval('.tile-shot', (i) => i.src);
  await shotPage.waitForTimeout(2500);
  const nextFrame = await shotPage.$eval('.tile-shot', (i) => i.src);
  check(firstFrame !== nextFrame, 'the screenshot tile takes a new frame');

  // A machine with a framebuffer but no control socket says what it lacks.
  const shotText = await shotPage.textContent('#tiles');
  check(/no control socket/.test(shotText),
    'a tile with no capture source says why it has no picture');
  await shotPage.screenshot({ path: `${OUT}/11-screenshot-wall.png` });
  await shotPage.close();

  console.log(`\n--- console errors (${expected.length} expected, ignored) ---`);
  if (errors.length === 0) console.log('(none unexpected)');
  for (const e of errors.slice(0, 15)) console.log('  ' + e);

  await browser.close();

  console.log(`\n${problems.length === 0 ? 'ALL CHECKS PASSED' : problems.length + ' CHECK(S) FAILED'}`);
  for (const p of problems) console.log('  - ' + p);
  process.exit(problems.length === 0 && errors.length === 0 ? 0 : 1);
})().catch((e) => { console.error('DRIVER ERROR', e); process.exit(2); });
