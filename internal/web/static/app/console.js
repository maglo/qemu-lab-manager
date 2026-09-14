// The framebuffer renderer.
//
// noVNC is loaded as native ES modules from /vendor/novnc (see its
// PROVENANCE.md). The websocket it opens is labview's /ws/console/{id},
// which proxies binary RFB to the machine's VNC port.

import RFB from '/vendor/novnc/core/rfb.js';
import { api, ws } from './api.js';
import { el } from './dom.js';

// RFBRenderer wraps one noVNC session.
//
// The session belongs to the renderer, not to wherever it is currently
// displayed. Expanding a machine moves the element into the viewport and
// leaves the session alone, so there is no reconnect and no second handshake
// (design section 7).
export class RFBRenderer {
  constructor(machine, { onState } = {}) {
    this.machine = machine;
    this.onState = onState || (() => {});
    this.element = el('div', { class: 'rfb-host' });
    this.rfb = null;
    this.state = 'connecting';
  }

  connect() {
    if (this.rfb) return;
    try {
      this.rfb = new RFB(this.element, ws.console(this.machine.id), { shared: true });
    } catch (err) {
      this.setState('failed', String(err));
      return;
    }

    // View-only until the lease says otherwise. The server enforces the
    // same rule, so this is the UI agreeing with it rather than the only
    // thing standing between a viewer and the keyboard.
    this.rfb.viewOnly = true;
    // Scale to whatever container it is in, so moving between a tile and
    // the viewport needs no reconnect.
    this.rfb.scaleViewport = true;
    this.rfb.clipViewport = false;
    this.rfb.resizeSession = false;
    this.rfb.background = '#000';
    this.rfb.showDotCursor = true;
    this.rfb.focusOnClick = false;

    this.rfb.addEventListener('connect', () => this.setState('connected'));
    this.rfb.addEventListener('disconnect', (ev) => {
      this.rfb = null;
      this.setState('disconnected', ev.detail?.clean ? '' : 'connection lost');
    });
    this.rfb.addEventListener('securityfailure', (ev) => {
      this.setState('failed', ev.detail?.reason || 'the console refused the connection');
    });
    this.rfb.addEventListener('desktopname', (ev) => {
      this.desktopName = ev.detail?.name;
    });
  }

  setState(state, detail = '') {
    this.state = state;
    this.onState(state, detail);
  }

  // setViewOnly is what "take control" does to the framebuffer. No
  // reconnect: the same session simply starts forwarding input, which the
  // server accepts because the lease is held.
  setViewOnly(viewOnly) {
    if (this.rfb) this.rfb.viewOnly = viewOnly;
  }

  sendCtrlAltDel() {
    if (this.rfb) this.rfb.sendCtrlAltDel();
  }

  focus() {
    if (this.rfb) this.rfb.focus();
  }

  destroy() {
    if (this.rfb) {
      this.rfb.disconnect();
      this.rfb = null;
    }
  }
}

// ScreenshotRenderer is the other half of section 7's tile seam.
//
// Beyond roughly eight tiles, every live RFB session is a decoder running in
// one browser tab, so the wall switches to periodic screenshots and opens RFB
// only on click. labview is started with -tile-mode=screenshot and the wall
// uses this renderer instead, which is a configuration change and not a
// rewrite.
//
// The frames come from QMP screendump through /api/machines/{id}/screenshot.
// One request is one frame, and the next request waits for the previous one
// to arrive: a machine that answers slowly then falls behind on its own
// instead of queueing requests the wall cannot draw.
export class ScreenshotRenderer {
  constructor(machine, { everyMs = 5000 } = {}) {
    this.machine = machine;
    this.everyMs = everyMs;
    this.timer = null;
    this.stopped = false;
    this.url = null;

    this.image = el('img', { class: 'tile-shot', alt: `Screen of ${machine.name}` });
    this.note = el('div', { class: 'tile-placeholder' }, 'Waiting for the first screenshot.');
    this.element = el('div', { class: 'shot-host' }, this.image, this.note);
    this.state = 'connecting';
  }

  connect() {
    if (!this.machine.hasControl) {
      this.fail('This machine has no control socket in the inventory, so labview cannot capture its screen.');
      return;
    }
    this.tick();
  }

  async tick() {
    if (this.stopped) return;
    try {
      const res = await fetch(api.screenshotURL(this.machine.id), { cache: 'no-store' });
      if (!res.ok) {
        const body = await res.json().catch(() => null);
        throw new Error((body && body.error) || `${res.status} ${res.statusText}`);
      }
      this.show(await res.blob());
    } catch (err) {
      this.fail(String(err.message || err));
    }
    if (!this.stopped) this.timer = setTimeout(() => this.tick(), this.everyMs);
  }

  show(blob) {
    const next = URL.createObjectURL(blob);
    // Revoke the frame this one replaces, or a tile leaks one blob per
    // interval for as long as the wall is open.
    if (this.url) URL.revokeObjectURL(this.url);
    this.url = next;
    this.image.src = next;
    this.image.hidden = false;
    this.note.hidden = true;
    this.state = 'connected';
  }

  fail(message) {
    this.note.textContent = message;
    this.note.hidden = false;
    this.image.hidden = true;
    this.state = 'failed';
  }

  setViewOnly() {}
  sendCtrlAltDel() {}
  focus() {}
  fit() {}

  destroy() {
    this.stopped = true;
    if (this.timer) clearTimeout(this.timer);
    if (this.url) URL.revokeObjectURL(this.url);
    this.url = null;
  }
}
