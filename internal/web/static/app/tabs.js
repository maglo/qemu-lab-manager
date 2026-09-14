// The details, recordings and activity tabs.
//
// The bias is toward showing everything the host already knows about a
// machine; the constraint is keeping it organised enough to be worth reading
// (design section 8). Every block is copyable, and the copied text is the
// plain original.

import { api } from './api.js';
import { el, clear, block, kvTable, copyBlock, bytes, when, ago } from './dom.js';

export function renderDetails(host, record) {
  clear(host);
  const d = record.details || {};

  if (d.warnings?.length) {
    host.append(el('ul', { class: 'warnings' },
      d.warnings.map((w) => el('li', { text: w }))));
  }

  // The command line first: section 8 calls it the single most useful thing
  // on the page and the most annoying to dig out by hand.
  if (d.commandLine?.length) {
    // A 40-argument QEMU invocation on one line is unreadable, and one
    // argument per line is nearly as bad. Pairing each flag with its value
    // is how a person reads a QEMU command line.
    const shell = d.commandLine.map(shellQuote).join(' ');
    const pretty = groupArgs(d.commandLine).join('\n');
    host.append(block('QEMU command line',
      copyBlock(pretty),
      el('details', {},
        el('summary', { class: 'muted', text: 'as one shell line' }),
        copyBlock(shell, { wrapped: true }))));
  }

  const u = d.unit || record.unit || {};
  if (u.name) {
    host.append(block('systemd unit', kvTable([
      ['Unit', u.name],
      ['Description', u.description],
      ['Load state', u.loadState],
      ['Active state', [u.activeState, u.subState].filter(Boolean).join(' / ')],
      ['Result', u.result],
      ['Changed', u.since ? `${when(u.since)} (${ago(u.since)})` : ''],
      ['Main PID', u.mainPid || ''],
    ])));
  }

  const hw = d.hardware || {};
  if (hw.memoryMb || hw.vcpus || hw.machineType || hw.firmware) {
    host.append(block('Hardware', kvTable([
      ['Memory', hw.memoryMb ? `${hw.memoryMb} MiB` : ''],
      ['vCPUs', hw.vcpus || ''],
      ['Machine type', hw.machineType],
      ['CPU model', hw.cpuModel],
      ['Firmware', hw.firmware],
    ])));
  }

  if (d.disks?.length) {
    host.append(block('Disks', ...d.disks.map((disk) => kvTable([
      ['Path', disk.path],
      ['Format', disk.format],
      ['Interface', disk.interface],
      ['Virtual size', disk.virtualSize ? bytes(disk.virtualSize) : ''],
      ['Actual size', disk.actualSize ? bytes(disk.actualSize) : ''],
      ['Backing file', disk.backingFile],
      ['Read only', disk.readOnly ? 'yes' : ''],
      ['Error', disk.error],
    ]))));
  }

  if (d.nics?.length) {
    host.append(block('Network', ...d.nics.map((nic) => kvTable([
      ['Kind', nic.kind],
      ['Model', nic.model],
      ['Tap', nic.tap],
      ['Bridge', nic.bridge],
      ['MAC', nic.mac],
      ['Guest addresses', (nic.addresses || []).join(', ')],
    ]))));
  }

  // The machine's entry in the inventory, verbatim.
  if (record.inventory) {
    host.append(block('Inventory entry',
      copyBlock(JSON.stringify(record.inventory, null, 2))));
  }

  if (!host.firstChild) {
    host.append(el('p', { class: 'empty', text: 'Nothing to show for this machine.' }));
  }
}

// groupArgs puts each flag and its value on one line. Anything it does not
// understand stays on a line of its own rather than being guessed at.
function groupArgs(args) {
  const lines = [];
  for (let i = 0; i < args.length; i++) {
    const arg = args[i];
    const next = args[i + 1];
    if (arg.startsWith('-') && next !== undefined && !next.startsWith('-')) {
      lines.push(`${arg} ${next}`);
      i++;
    } else {
      lines.push(arg);
    }
  }
  return lines;
}

// shellQuote is for display only: it makes the "as one shell line" form
// copy-pasteable without pretending to be a shell parser.
function shellQuote(arg) {
  if (/^[A-Za-z0-9_@%+=:,./-]+$/.test(arg)) return arg;
  return `'${arg.replace(/'/g, `'\\''`)}'`;
}

export function renderRecordings(host, machineID, recordings, { note } = {}) {
  clear(host);
  if (note) {
    host.append(el('p', { class: 'note warn', text: note }));
    return;
  }
  if (!recordings?.length) {
    host.append(el('p', { class: 'empty', text: 'No serial captures for this machine yet.' }));
    return;
  }

  host.append(block('Serial captures',
    el('p', { class: 'muted', text:
      'One file per VM run, in asciicast v2 format. A reconnect is a VM ' +
      'lifecycle boundary, so each file covers one run.' }),
    el('div', { class: 'rows' }, recordings.map((r) => el('div', { class: 'row' },
      el('time', { datetime: r.started || '', text: when(r.started) || r.name }),
      el('span', { class: 'what', text: bytes(r.size) }),
      el('span', { class: 'spacer' }),
      el('a', { href: api.recordingURL(machineID, r.name, true), target: '_blank', rel: 'noopener' }, 'view'),
      el('a', { href: api.recordingURL(machineID, r.name), download: r.name }, 'download'))))));
}

export function renderActivity(host, events) {
  clear(host);
  if (!events?.length) {
    host.append(el('p', { class: 'empty', text: 'Nothing recorded for this machine yet.' }));
    return;
  }
  host.append(block('Activity',
    el('div', { class: 'rows' }, events.map((ev) => el('div', { class: 'row' },
      el('time', { datetime: ev.at || '', text: when(ev.at) }),
      el('span', { class: 'who', text: ev.user || '' }),
      el('span', { class: 'what', text: [ev.action, ev.channel, ev.detail].filter(Boolean).join(' · ') }))))));
}
