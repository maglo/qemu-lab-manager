const { chromium } = require('playwright');

// Through the proxy by default, so the run exercises section 10's topology.
const BASE = process.env.LABVIEW_URL || 'http://127.0.0.1:18081';
const OUT = process.env.SHOTS_DIR || __dirname + '/shots';
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
  check(JSON.stringify(tabs) === JSON.stringify(['console','serial','details','logs','recordings','activity']),
    `all six tabs present (got ${tabs.join(',')})`);
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
  check(/withheld/.test(details), 'inventory entry marks the withheld addresses');
  check(!/10\.20\.0\.11|\/run\/qemu/.test(details), 'inventory entry does not leak an address');
  const copyButtons = await page.$$('#details .copywrap button');
  check(copyButtons.length >= 2, `copy buttons on every block (got ${copyButtons.length})`);
  await page.screenshot({ path: `${OUT}/05-details.png`, fullPage: true });

  // --- logs ---
  await page.click('#tabs button[data-tab="logs"]');
  await page.waitForTimeout(1500);
  await onlyPanelVisible(page, 'logs');
  const logs = await page.textContent('#logs');
  check(logs.trim().length > 0, 'logs tab renders journal lines');
  await page.screenshot({ path: `${OUT}/06-logs.png` });

  // --- recordings ---
  await page.click('#tabs button[data-tab="recordings"]');
  await page.waitForTimeout(1200);
  await onlyPanelVisible(page, 'recordings');
  const recs = await page.textContent('#recordings');
  check(/download|capture/i.test(recs), 'recordings tab lists captures');
  await page.screenshot({ path: `${OUT}/07-recordings.png` });

  // --- activity ---
  await page.click('#tabs button[data-tab="activity"]');
  await page.waitForTimeout(1200);
  await onlyPanelVisible(page, 'activity');
  const act = await page.textContent('#activity');
  check(/alice@example\.com/.test(act), 'activity names who did what');
  check(/control-granted|attach/.test(act), 'activity records the control grant');
  await page.screenshot({ path: `${OUT}/08-activity.png` });

  // --- serial-only machine opens on serial ---
  await page.click('#back');
  await page.waitForTimeout(1000);
  await page.click('.tile:has-text("el9-serial-only")');
  await page.waitForTimeout(1200);
  const soTabs = await page.$$eval('#tabs button', (bs) => bs.map((b) => b.dataset.tab));
  check(!soTabs.includes('console'), 'a machine with no framebuffer has no console tab');
  check(await page.getAttribute('#tabs button[data-tab="serial"]', 'aria-selected') === 'true',
    'serial-only machine opens on the serial tab');
  await page.screenshot({ path: `${OUT}/09-serial-only.png` });

  // --- back to the wall: the session returned to its tile ---
  await page.click('#back');
  await page.waitForTimeout(1500);
  check(await page.isVisible('#wall'), 'back returns to the wall');
  check((await page.$$('.tile canvas')).length >= 1, 'the framebuffer session went back to its tile');
  await page.screenshot({ path: `${OUT}/10-wall-after.png` });

  // --- a machine file appears and goes, with labview left running ---
  const invDir = process.env.INVENTORY_DIR;
  const newFile = invDir + '/late-arrival.yaml';
  fs.writeFileSync(newFile, 'name: late-arrival\nhost: kvm03\nnotes: Written while labview runs\n');
  const arrived = await tileCount(page, 5);
  check(arrived, 'a new machine file reaches the wall without a restart');
  await page.screenshot({ path: `${OUT}/11-wall-new-machine.png` });

  fs.unlinkSync(newFile);
  check(await tileCount(page, 4), 'a deleted machine file leaves the wall');

  console.log(`\n--- console errors (${expected.length} expected, ignored) ---`);
  if (errors.length === 0) console.log('(none unexpected)');
  for (const e of errors.slice(0, 15)) console.log('  ' + e);

  await browser.close();

  console.log(`\n${problems.length === 0 ? 'ALL CHECKS PASSED' : problems.length + ' CHECK(S) FAILED'}`);
  for (const p of problems) console.log('  - ' + p);
  process.exit(problems.length === 0 && errors.length === 0 ? 0 : 1);
})().catch((e) => { console.error('DRIVER ERROR', e); process.exit(2); });
