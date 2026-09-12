// The serial view.
//
// Two frame types arrive on this socket and they must not be confused: binary
// frames are what the VM printed, text frames are the viewer talking about
// itself (design section 11). Output goes to the terminal; status goes to the
// note line under it, never into the scrollback.

import { ws } from './api.js';
import { el } from './dom.js';

// SerialView owns one terminal and one websocket.
export class SerialView {
  constructor(machine, { onStatus, fontSize = 13, scrollback = 5000 } = {}) {
    this.machine = machine;
    this.onStatus = onStatus || (() => {});
    this.element = el('div', { class: 'xterm-host' });
    this.socket = null;
    this.closed = false;

    this.term = new window.Terminal({
      fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, monospace',
      fontSize,
      scrollback,
      convertEol: false,
      cursorBlink: false,
      theme: { background: '#000000', foreground: '#d9dde3' },
    });
    this.term.open(this.element);

    // Keystrokes go out as binary, because that is the only thing the
    // server treats as input. Whether they reach the machine depends on the
    // lease, which the server checks; a refusal comes back as a status
    // frame rather than silence.
    this.term.onData((data) => this.send(data));
  }

  connect(write = false) {
    if (this.socket) return;
    const socket = new WebSocket(ws.serial(this.machine.id, write));
    socket.binaryType = 'arraybuffer';
    this.socket = socket;

    socket.onmessage = (ev) => {
      if (typeof ev.data === 'string') {
        // Text frame: viewer status.
        try {
          this.onStatus(JSON.parse(ev.data));
        } catch {
          // Not our business to render malformed status.
        }
        return;
      }
      // Binary frame: raw bytes from the machine. xterm decodes UTF-8
      // incrementally across writes, which is what the design asks for --
      // a serial stream can split a rune across reads and the server must
      // not decode on its behalf.
      this.term.write(new Uint8Array(ev.data));
    };

    socket.onclose = (ev) => {
      this.socket = null;
      if (this.closed) return;
      this.onStatus({
        type: 'down',
        message: ev.reason || 'serial connection closed',
      });
    };
    socket.onerror = () => {
      this.onStatus({ type: 'down', message: 'serial connection failed' });
    };
  }

  send(data) {
    if (!this.socket || this.socket.readyState !== WebSocket.OPEN) return;
    this.socket.send(new TextEncoder().encode(data));
  }

  // fit sizes the terminal to its container. Done by measuring a character
  // rather than pulling in the fit addon, which is one more vendored file
  // for a dozen lines of arithmetic.
  fit() {
    const host = this.element;
    if (!host.clientWidth || !host.clientHeight) return;
    const dims = this.term._core?._renderService?.dimensions?.css?.cell;
    if (!dims || !dims.width || !dims.height) return;
    const cols = Math.max(20, Math.floor(host.clientWidth / dims.width));
    const rows = Math.max(4, Math.floor(host.clientHeight / dims.height));
    if (cols !== this.term.cols || rows !== this.term.rows) {
      this.term.resize(cols, rows);
    }
  }

  setWritable(writable) {
    this.term.options.cursorBlink = writable;
    this.term.options.disableStdin = !writable;
  }

  focus() {
    this.term.focus();
  }

  destroy() {
    this.closed = true;
    if (this.socket) {
      this.socket.close();
      this.socket = null;
    }
    this.term.dispose();
  }
}
