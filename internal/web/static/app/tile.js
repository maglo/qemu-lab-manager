// A wall tile.
//
// The tile is built so that what it renders is a choice rather than a
// structure: a live framebuffer, a periodic screenshot, or a serial tail for
// a machine with no framebuffer worth showing. Section 7 asks for exactly
// that, so switching the wall to screenshots past eight tiles is a
// configuration change and not a rewrite.

import { RFBRenderer, ScreenshotRenderer } from './console.js';
import { SerialView } from './serial.js';
import { el, clear } from './dom.js';

export class Tile {
  constructor(summary, { tileMode, screenshotEveryMs, onOpen }) {
    this.summary = summary;
    this.tileMode = tileMode;
    this.screenshotEveryMs = screenshotEveryMs;
    this.onOpen = onOpen;
    this.renderer = null;

    this.dot = el('span', { class: 'dot' });
    this.stateLabel = el('span', { class: 'pill' });

    // The screen element is the movable part: expanding a machine moves it
    // into the viewport, which is what keeps the session alive.
    this.screen = el('div', { class: 'tile-screen' });

    // What the renderer is doing, over the picture it draws. A framebuffer
    // that lost its connection keeps the last frame it painted, which reads
    // as a live screen that happens to be still, so the renderer's own state
    // has to be visible. The note lives inside the screen, so it follows it
    // into the expanded view.
    this.note = el('div', { class: 'screen-note', hidden: true });
    this.screen.append(this.note);

    // A transparent hit target over the screen.
    //
    // noVNC installs its own pointer handlers on the canvas and stops them
    // propagating, so without this a click on the framebuffer -- the most
    // natural place to click a tile -- never reaches the tile. It also
    // means a stray click on the wall cannot reach a VM, which matters
    // because the wall is view-only for everyone, always (design
    // section 4).
    //
    // It lives in the slot rather than inside the screen, so that moving
    // the screen into the expanded view leaves the cover behind and the
    // framebuffer becomes clickable exactly when it becomes drivable.
    this.cover = el('div', { class: 'tile-cover', 'aria-hidden': 'true' });

    this.slot = el('div', { class: 'tile-screen-slot' });
    this.slot.append(this.screen, this.cover);

    this.element = el('div', {
      class: 'tile',
      tabindex: '0',
      role: 'button',
      'aria-label': `Open ${summary.name}`,
      onclick: () => this.onOpen(this.summary.id),
      onkeydown: (ev) => {
        if (ev.key === 'Enter' || ev.key === ' ') {
          ev.preventDefault();
          this.onOpen(this.summary.id);
        }
      },
    },
      el('div', { class: 'tile-head' },
        this.dot,
        el('span', { class: 'tile-name', text: summary.name }),
        el('span', { class: 'tile-host', text: summary.host || '' }),
        el('span', { class: 'spacer' }),
        this.stateLabel),
      this.slot);

    this.update(summary);
  }

  // mount starts whatever this tile renders.
  mount() {
    if (this.renderer) return;

    if (this.summary.hasConsole) {
      this.renderer = this.tileMode === 'screenshot'
        ? new ScreenshotRenderer(this.summary, { everyMs: this.screenshotEveryMs })
        : new RFBRenderer(this.summary, {
          onState: (state, detail) => this.paintScreen(state, detail),
        });
    } else if (this.summary.hasSerial) {
      // Serial-only: the tile is a serial tail (design section 7).
      this.renderer = new SerialTailRenderer(this.summary);
    } else {
      // A machine with neither still appears on the wall, saying why.
      this.renderer = new NoConsoleRenderer(this.summary);
    }

    this.screen.prepend(this.renderer.element);
    this.renderer.connect();
  }

  update(summary) {
    this.summary = summary;
    this.paintState();
  }

  // paintScreen reports what the renderer is doing. Only a state that is not
  // "connected" says anything: a working framebuffer needs no label.
  paintScreen(state, detail) {
    const text = {
      connecting: 'connecting to the console',
      reconnecting: `console lost${detail ? `: ${detail}` : ''}; connecting again`,
      failed: detail || 'the console is not available',
    }[state];
    this.note.textContent = text || '';
    this.note.hidden = !text;
    this.note.className = 'screen-note' + (state === 'failed' ? ' bad' : '');
  }

  paintState() {
    const s = this.summary;
    this.dot.className = 'dot ' + (s.live ? 'live' : s.why ? 'dead' : 'unknown');

    let label = s.live ? 'up' : (s.why || 'unknown');
    let cls = 'pill';
    if (s.lease && s.lease.holder) {
      label = `${s.lease.holder} has control`;
      cls = 'pill held';
    }
    this.stateLabel.textContent = label;
    this.stateLabel.className = cls;
    this.element.title = s.why || '';
  }

  // detachScreen hands the screen element to the expanded view. The session
  // keeps running; only its parent changes.
  detachScreen() {
    return this.screen;
  }

  // reattachScreen takes it back when returning to the wall.
  reattachScreen() {
    this.slot.append(this.screen);
  }

  setViewOnly(viewOnly) {
    this.renderer?.setViewOnly?.(viewOnly);
  }

  sendCtrlAltDel() {
    this.renderer?.sendCtrlAltDel?.();
  }

  focus() {
    this.renderer?.focus?.();
  }

  fit() {
    this.renderer?.fit?.();
  }

  destroy() {
    this.renderer?.destroy?.();
    this.renderer = null;
    this.element.remove();
  }
}

// SerialTailRenderer shows a machine's serial output in a tile, read-only.
class SerialTailRenderer {
  constructor(machine) {
    this.machine = machine;
    this.view = new SerialView(machine, { fontSize: 10, scrollback: 500 });
    this.element = this.view.element;
  }

  connect() {
    this.view.connect(false);
    this.view.setWritable(false);
    // Let the element get its size before measuring it.
    requestAnimationFrame(() => this.view.fit());
  }

  fit() { this.view.fit(); }
  setViewOnly() {}
  sendCtrlAltDel() {}
  focus() {}
  destroy() { this.view.destroy(); }
}

// NoConsoleRenderer explains a machine with neither channel, rather than
// leaving a black rectangle on the wall (design section 7).
class NoConsoleRenderer {
  constructor(machine) {
    this.element = el('div', { class: 'tile-placeholder' },
      machine.why || 'This machine has no console and no serial line in the inventory.');
  }

  connect() {}
  setViewOnly() {}
  sendCtrlAltDel() {}
  focus() {}
  destroy() {}
}

export { clear };
