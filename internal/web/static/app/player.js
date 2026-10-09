// The recording player.
//
// A capture is an asciicast v2 file: a header line, then one line per output
// event as [elapsed, "o", text] (design section 11). Replaying it needs a
// terminal and a clock, and labview already vendors the terminal the serial
// tab uses, so the player is arithmetic rather than a second dependency.

import { api } from './api.js';
import { el } from './dom.js';

// A capture of a boot holds minutes where nothing is printed. Replaying those
// in real time means watching a still screen, so a gap longer than this is
// played as this. The player says that it does it.
const IDLE_CAP = 2;

// RecordingPlayer replays one capture in a terminal.
//
// The terminal is written to in order and never rewound, because a terminal is
// the sum of everything written to it. Seeking backwards therefore resets it
// and replays from the start, which is fast: it is writes with no waiting.
export class RecordingPlayer {
  constructor(machineID, recording, { onClose } = {}) {
    this.machineID = machineID;
    this.recording = recording;
    this.onClose = onClose || (() => {});

    this.events = [];
    this.total = 0;
    this.at = 0;
    this.next = 0;
    this.playing = false;
    this.frame = null;
    this.last = 0;

    this.term = null;
    this.screen = el('div', { class: 'player-screen' });
    this.status = el('span', { class: 'player-status muted' }, 'Loading…');
    this.playButton = el('button', {
      type: 'button', class: 'ghost', text: 'Pause',
      onclick: () => this.toggle(),
    });
    this.bar = el('div', { class: 'player-bar-fill' });
    this.track = el('div', {
      class: 'player-bar',
      role: 'progressbar',
      title: 'Click to move through the capture',
      onclick: (ev) => this.scrub(ev),
    }, this.bar);

    this.element = el('div', { class: 'player' },
      el('div', { class: 'player-head' },
        el('strong', { text: recording.name }),
        el('span', { class: 'spacer' }),
        this.status,
        this.playButton,
        el('button', {
          type: 'button', class: 'ghost', text: 'Restart',
          onclick: () => this.restart(),
        }),
        el('button', {
          type: 'button', class: 'ghost', text: 'Close',
          onclick: () => this.onClose(),
        })),
      this.screen,
      this.track);
  }

  async load() {
    let text;
    try {
      const res = await fetch(api.recordingURL(this.machineID, this.recording.name, true),
        { cache: 'no-store' });
      if (!res.ok) {
        const body = await res.json().catch(() => null);
        throw new Error((body && body.error) || `${res.status} ${res.statusText}`);
      }
      text = await res.text();
    } catch (err) {
      this.fail(`Could not read this capture: ${err.message || err}`);
      return;
    }

    const header = this.parse(text);
    if (!header) return;

    this.term = new window.Terminal({
      fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
      fontSize: 13,
      cols: header.width || 80,
      rows: header.height || 24,
      scrollback: 1000,
      disableStdin: true,
      cursorBlink: false,
      theme: { background: '#000000', foreground: '#d9dde3' },
    });
    this.term.open(this.screen);
    this.play();
  }

  // parse reads the asciicast and returns its header.
  //
  // A capture of a running machine is still being written, so the last line
  // can be half a line. Parsing stops at the first line it cannot read rather
  // than refusing the whole file.
  parse(text) {
    const lines = text.split('\n');
    let header;
    try {
      header = JSON.parse(lines[0]);
    } catch {
      this.fail('This file is not a capture labview can read.');
      return null;
    }
    if (header.version !== 2) {
      this.fail(`This capture is asciicast v${header.version}, and labview reads v2.`);
      return null;
    }

    // The clock of the file is real time. The clock of the player closes a
    // gap longer than IDLE_CAP, so the arithmetic keeps both.
    let previous = 0;
    let clock = 0;
    let idle = 0;
    for (let i = 1; i < lines.length; i++) {
      if (!lines[i]) continue;
      let event;
      try {
        event = JSON.parse(lines[i]);
      } catch {
        break; // A partial line: the machine is still running.
      }
      if (!Array.isArray(event) || event[1] !== 'o') continue;
      const gap = Math.max(0, event[0] - previous);
      if (gap > IDLE_CAP) idle += gap - IDLE_CAP;
      clock += Math.min(gap, IDLE_CAP);
      previous = event[0];
      this.events.push({ at: clock, data: event[2] });
    }
    this.total = clock;
    this.idle = idle;
    this.recorded = previous;
    if (!this.events.length) {
      this.fail('This capture holds no output.');
      return null;
    }
    return header;
  }

  fail(message) {
    this.status.textContent = '';
    this.screen.append(el('p', { class: 'note bad', text: message }));
    this.playButton.disabled = true;
  }

  play() {
    if (this.playing || !this.term) return;
    if (this.total > 0 && this.at >= this.total) this.restart();
    this.playing = true;
    this.playButton.textContent = 'Pause';
    this.last = performance.now();
    this.frame = requestAnimationFrame((t) => this.tick(t));
  }

  pause() {
    this.playing = false;
    this.playButton.textContent = 'Play';
    if (this.frame) cancelAnimationFrame(this.frame);
    this.frame = null;
  }

  toggle() {
    if (this.playing) this.pause();
    else this.play();
  }

  restart() {
    this.term?.reset();
    this.at = 0;
    this.next = 0;
    this.paint();
    if (!this.playing) this.play();
  }

  tick(now) {
    if (!this.playing) return;
    this.at = Math.min(this.total, this.at + (now - this.last) / 1000);
    this.last = now;
    this.drain();
    this.paint();
    if (this.at >= this.total) {
      this.pause();
      this.playButton.textContent = 'Replay';
      return;
    }
    this.frame = requestAnimationFrame((t) => this.tick(t));
  }

  // drain writes every event the clock has reached.
  drain() {
    while (this.next < this.events.length && this.events[this.next].at <= this.at) {
      this.term.write(this.events[this.next].data);
      this.next++;
    }
  }

  // scrub moves to the point of the track that was clicked. Moving backwards
  // resets the terminal, because the screen is the sum of what came before.
  scrub(ev) {
    if (!this.term || !this.total) return;
    const box = this.track.getBoundingClientRect();
    const target = Math.max(0, Math.min(1, (ev.clientX - box.left) / box.width)) * this.total;
    if (target < this.at) {
      this.term.reset();
      this.next = 0;
    }
    this.at = target;
    this.drain();
    this.paint();
  }

  paint() {
    const pct = this.total ? (this.at / this.total) * 100 : 0;
    this.bar.style.width = `${pct.toFixed(2)}%`;
    this.track.setAttribute('aria-valuenow', Math.round(this.at));
    const idle = this.idle >= 1 ? `, ${clock(this.idle)} of waiting closed up` : '';
    this.status.textContent = `${clock(this.at)} / ${clock(this.total)}${idle}`;
  }

  destroy() {
    this.pause();
    this.term?.dispose();
    this.term = null;
  }
}

function clock(seconds) {
  const whole = Math.max(0, Math.round(seconds));
  const mins = Math.floor(whole / 60);
  const secs = whole % 60;
  return mins ? `${mins}m ${String(secs).padStart(2, '0')}s` : `${secs}s`;
}
