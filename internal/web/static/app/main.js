// labview's browser application.
//
// Three states and no more (design section 7): the wall, a machine expanded
// to the viewport, and the serial-only variant of expanded, which differs
// only in which tab opens first.

import { api } from './api.js';
import { Tile } from './tile.js';
import { SerialView } from './serial.js';
import { renderDetails, renderRecordings, renderActivity } from './tabs.js';
import { $, el, clear, toast, when } from './dom.js';

const WALL_POLL_MS = 4000;

const state = {
  config: { tileMode: 'rfb', identity: '', leaseIdleSeconds: 180, recordings: true },
  summaries: new Map(),
  tiles: new Map(),
  open: null,        // machine id, when expanded
  tab: 'console',
  record: null,      // last fetched whole record for the open machine
  serial: null,      // SerialView for the expanded serial tab
  haveControl: false,
  pollTimer: null,
};

// --- startup ----------------------------------------------------------

async function start() {
  try {
    state.config = await api.config();
  } catch (err) {
    toast(`labview is not answering: ${err.message}`, 'bad');
    return;
  }
  $('#identity').textContent = state.config.identity || '';

  wireControls();
  window.addEventListener('hashchange', applyRoute);
  window.addEventListener('resize', () => {
    state.tiles.get(state.open)?.fit();
    state.serial?.fit();
  });

  await refreshWall();
  applyRoute();
  state.pollTimer = setInterval(refreshWall, WALL_POLL_MS);
}

// --- routing ----------------------------------------------------------
//
// The hash carries the state so a developer can link a colleague straight to
// a machine and the tab that shows what they mean.

function applyRoute() {
  const hash = location.hash.replace(/^#\/?/, '');
  if (!hash) {
    showWall();
    return;
  }
  const [, id, tab] = hash.match(/^machine\/([^/]+)(?:\/(.+))?$/) || [];
  if (!id || !state.summaries.has(id)) {
    showWall();
    return;
  }
  openMachine(id, tab);
}

function navigate(id, tab) {
  location.hash = id ? `#/machine/${encodeURIComponent(id)}/${tab || defaultTab(id)}` : '';
}

// defaultTab implements the serial-only state: a machine with no framebuffer
// worth showing opens on serial instead of console. Same view, different
// default (design section 7).
function defaultTab(id) {
  const s = state.summaries.get(id);
  if (s && !s.hasConsole && s.hasSerial) return 'serial';
  return 'console';
}

// --- the wall ---------------------------------------------------------

async function refreshWall() {
  let machines;
  try {
    ({ machines } = await api.machines());
  } catch (err) {
    toast(`Could not read the machine list: ${err.message}`, 'bad');
    return;
  }

  const seen = new Set();
  for (const summary of machines) {
    seen.add(summary.id);
    state.summaries.set(summary.id, summary);

    let tile = state.tiles.get(summary.id);
    if (!tile) {
      tile = new Tile(summary, {
        tileMode: state.config.tileMode,
        onOpen: (id) => navigate(id),
      });
      state.tiles.set(summary.id, tile);
      $('#tiles').append(tile.element);
      // Only start a session for a tile that is actually on the wall. The
      // expanded view moves the element, so a tile whose machine is open
      // still needs its session.
      tile.mount();
    } else {
      tile.update(summary);
    }
  }

  // Machines that left the inventory leave the wall.
  for (const [id, tile] of state.tiles) {
    if (!seen.has(id)) {
      tile.destroy();
      state.tiles.delete(id);
      state.summaries.delete(id);
      if (state.open === id) {
        toast('This machine left the inventory', 'bad');
        navigate(null);
      }
    }
  }

  $('#wall-empty').hidden = machines.length > 0;

  if (state.open) paintExpandedHeader();
}

function showWall() {
  if (state.open) closeMachine();
  $('#wall').hidden = false;
  $('#expanded').hidden = true;
  $('#back').hidden = true;
  $('#bar-machine').hidden = true;
  $('#title').textContent = 'labview';
}

// --- expanded ---------------------------------------------------------

async function openMachine(id, tab) {
  const summary = state.summaries.get(id);
  if (!summary) return;

  if (state.open && state.open !== id) closeMachine();

  const first = state.open !== id;
  state.open = id;
  state.tab = tab || defaultTab(id);

  $('#wall').hidden = true;
  $('#expanded').hidden = false;
  $('#back').hidden = false;
  $('#bar-machine').hidden = false;

  if (first) {
    // Move the tile's screen element into the viewport. The RFB session
    // inside it keeps running, so there is no reconnect and no second
    // handshake (design section 7).
    const tile = state.tiles.get(id);
    if (tile && summary.hasConsole) {
      $('#console-host').append(tile.detachScreen());
    }
    buildTabs(summary);
  }

  paintExpandedHeader();
  selectTab(state.tab);
  loadRecord(id);
}

function closeMachine() {
  const id = state.open;
  state.open = null;
  state.record = null;
  state.haveControl = false;

  // Give the screen element back to its tile, still connected.
  const tile = state.tiles.get(id);
  if (tile) {
    tile.setViewOnly(true);
    tile.reattachScreen();
    tile.fit();
  }

  if (state.serial) {
    state.serial.destroy();
    state.serial = null;
  }
  clear($('#console-host'));
}

function paintExpandedHeader() {
  const s = state.summaries.get(state.open);
  if (!s) return;

  $('#title').textContent = s.name;
  $('#machine-host').textContent = s.host || '';

  const stateEl = $('#machine-state');
  stateEl.textContent = s.live ? 'up' : (s.why || 'unknown');
  stateEl.className = 'pill';

  const mine = s.lease && s.lease.holder === state.config.identity;
  state.haveControl = Boolean(mine);

  const control = $('#control-state');
  if (s.lease && s.lease.holder) {
    control.textContent = mine
      ? `you have control until ${when(s.lease.expires)}`
      : `${s.lease.holder} has control`;
  } else {
    control.textContent = 'nobody has control';
  }

  $('#take').hidden = Boolean(mine);
  $('#take').disabled = Boolean(s.lease && s.lease.holder && !mine);
  $('#release').hidden = !mine;

  // Everything that drives the machine needs the lease.
  for (const sel of ['#cad', '#power-start', '#power-stop', '#power-restart']) {
    $(sel).disabled = !mine;
  }
  for (const sel of ['#power-start', '#power-stop', '#power-restart']) {
    if (!s.canPower) {
      $(sel).disabled = true;
      $(sel).title = 'This machine has no systemd unit in the inventory, so labview will not manage it';
    } else {
      $(sel).title = '';
    }
  }
  $('#cad').disabled = !mine || !s.hasConsole;

  // The framebuffer follows the lease: view-only until control is held.
  state.tiles.get(state.open)?.setViewOnly(!mine);
  state.serial?.setWritable(Boolean(mine));
}

// --- tabs -------------------------------------------------------------

const ALL_TABS = [
  ['console', 'Console'],
  ['serial', 'Serial'],
  ['details', 'Details'],
  ['recordings', 'Recordings'],
  ['activity', 'Activity'],
];

function buildTabs(summary) {
  const nav = $('#tabs');
  clear(nav);
  for (const [id, label] of ALL_TABS) {
    if (id === 'console' && !summary.hasConsole) continue;
    if (id === 'serial' && !summary.hasSerial) continue;
    if (id === 'recordings' && !state.config.recordings) continue;
    nav.append(el('button', {
      type: 'button',
      role: 'tab',
      'data-tab': id,
      text: label,
      onclick: () => navigate(summary.id, id),
    }));
  }
}

function selectTab(tab) {
  // A machine with no console cannot show one.
  const summary = state.summaries.get(state.open);
  if (tab === 'console' && summary && !summary.hasConsole) tab = defaultTab(state.open);
  state.tab = tab;

  for (const button of $('#tabs').children) {
    button.setAttribute('aria-selected', String(button.dataset.tab === tab));
  }
  for (const panel of document.querySelectorAll('.panel')) {
    panel.hidden = panel.dataset.tab !== tab;
  }

  if (tab === 'console') {
    state.tiles.get(state.open)?.fit();
    if (state.haveControl) state.tiles.get(state.open)?.focus();
  }
  if (tab === 'serial') openSerial();
  if (tab === 'recordings') loadRecordings();
}

async function loadRecord(id) {
  try {
    const record = await api.machine(id);
    if (state.open !== id) return;
    state.record = record;
    renderDetails($('#details'), record);
    renderActivity($('#activity'), record.activity);
  } catch (err) {
    if (state.open !== id) return;
    clear($('#details'));
    $('#details').append(el('p', { class: 'note bad', text: `Could not read this machine: ${err.message}` }));
  }
}

// --- serial tab -------------------------------------------------------

function openSerial() {
  const summary = state.summaries.get(state.open);
  if (!summary?.hasSerial) return;

  if (!state.serial) {
    state.serial = new SerialView(summary, { onStatus: paintSerialStatus });
    $('#serial-host').append(state.serial.element);
    // Attach read-only. Control is taken through the lease endpoint, so the
    // same socket starts carrying input once it is held -- no reconnect.
    state.serial.connect(false);
    state.serial.setWritable(state.haveControl);
  }
  requestAnimationFrame(() => {
    state.serial?.fit();
    if (state.haveControl) state.serial?.focus();
  });
}

// paintSerialStatus renders a text frame. It never touches the terminal:
// status must not land in the scrollback or in anything grepping the output
// (design section 11).
function paintSerialStatus(msg) {
  const note = $('#serial-note');
  const parts = [];
  switch (msg.type) {
    case 'up': parts.push('machine is connected'); break;
    case 'down': parts.push(msg.message || 'machine is not connected'); break;
    case 'attached': parts.push(msg.message || 'attached'); break;
    case 'lagged': parts.push(msg.message || 'fell behind; reload to resync'); break;
    case 'control': parts.push(msg.message || ''); break;
    default: parts.push(msg.message || msg.type);
  }
  note.textContent = parts.filter(Boolean).join(' — ');
  note.className = 'note' +
    (msg.type === 'down' || msg.type === 'lagged' ? ' bad' : '') +
    (msg.type === 'control' ? ' warn' : '');

  // "Your control is about to expire" deserves more than a note line.
  if (msg.type === 'control' && /about to expire/.test(msg.message || '')) {
    toast(msg.message, 'bad');
  }
  // A lease change on this machine changes what the buttons should offer.
  if (msg.type === 'control') refreshWall();
}

// --- recordings tab ---------------------------------------------------

async function loadRecordings() {
  const host = $('#recordings');
  if (!state.config.recordings) {
    renderRecordings(host, state.open, null, { note: 'Serial capture is disabled on this labview.' });
    return;
  }
  try {
    const { recordings } = await api.recordings(state.open);
    renderRecordings(host, state.open, recordings);
  } catch (err) {
    renderRecordings(host, state.open, null, { note: `Could not list recordings: ${err.message}` });
  }
}

// --- controls ---------------------------------------------------------

function wireControls() {
  $('#back').onclick = () => navigate(null);

  $('#take').onclick = async () => {
    try {
      await api.lease(state.open, 'acquire');
      toast('You have control', 'good');
    } catch (err) {
      toast(err.holder ? `${err.holder} is driving this machine` : err.message, 'bad');
    }
    await refreshWall();
    if (state.tab === 'console') state.tiles.get(state.open)?.focus();
    if (state.tab === 'serial') state.serial?.focus();
  };

  $('#release').onclick = async () => {
    try {
      await api.lease(state.open, 'release');
    } catch (err) {
      toast(err.message, 'bad');
    }
    await refreshWall();
  };

  $('#cad').onclick = () => state.tiles.get(state.open)?.sendCtrlAltDel();

  $('#fullscreen').onclick = () => {
    const panel = document.querySelector('.panel:not([hidden])');
    if (!document.fullscreenElement) panel?.requestFullscreen?.();
    else document.exitFullscreen?.();
  };

  $('#power-start').onclick = () => power('start');
  $('#power-stop').onclick = () => power('stop', true);
  // Restart is the only destructive thing in an otherwise read-mostly tool,
  // so it confirms (design section 12).
  $('#power-restart').onclick = () => power('restart', true);
}

async function power(op, confirmFirst = false) {
  const summary = state.summaries.get(state.open);
  if (!summary) return;
  if (confirmFirst && !window.confirm(
    `${op === 'stop' ? 'Stop' : 'Restart'} ${summary.name}?\n\n` +
    'Anything running on this machine will be interrupted.')) {
    return;
  }

  try {
    await api.power(state.open, op);
    toast(`${op} submitted`, 'good');
  } catch (err) {
    toast(err.holder ? `${err.holder} is driving this machine` : err.message, 'bad');
  }
  // systemd takes a moment; look again shortly.
  await refreshWall();
  setTimeout(refreshWall, 1200);
  setTimeout(() => { if (state.open) loadRecord(state.open); }, 1500);
}

start();
