// The framebuffer renderer.
//
// noVNC is loaded as native ES modules from /vendor/novnc (see its
// PROVENANCE.md). The websocket it opens is labview's /ws/console/{id},
// which proxies binary RFB to the machine's VNC port.

import RFB from '/vendor/novnc/core/rfb.js';
import { ws } from './api.js';
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
// one browser tab, so the wall should switch to periodic screenshots and open
// RFB only on click. The tile is therefore built so that this is a
// configuration change and not a rewrite -- labview is started with
// -tile-mode=screenshot and the wall uses this renderer instead.
//
// The image source is the missing piece, and deliberately so: taking a
// screenshot server-side means either QMP screendump, which design section 12
// ruled out in favour of systemd, or decoding RFB in the server, which is a
// framebuffer decoder labview does not otherwise need. Neither is settled, so
// this renderer says what it is waiting for rather than inventing an endpoint.
export class ScreenshotRenderer {
  constructor(machine) {
    this.machine = machine;
    this.element = el('div', { class: 'tile-placeholder' },
      'Screenshot tiles need a capture source on the hypervisor, ' +
      'which is not implemented yet. Click to open the live console.');
    this.state = 'placeholder';
  }

  connect() {}
  setViewOnly() {}
  sendCtrlAltDel() {}
  focus() {}
  destroy() {}
}
