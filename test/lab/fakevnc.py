#!/usr/bin/env python3
"""A minimal RFB 3.8 server: handshake, one solid framebuffer, nothing else.

Enough to prove that labview's console proxy, its RFB input filter and the
vendored noVNC all agree on the protocol.

The server offers the two pseudo-encodings that QEMU offers, because noVNC
sends different messages once it has them: keys become message 255 and pointer
events grow a byte. A fake that offers neither tests a client that the lab
never runs (https://github.com/maglo/qemu-lab-manager/issues/51).
"""
import socket, struct, sys, threading, time

W, H = 640, 480
NAME = b"fake-el9-build"

# The pseudo-encodings QEMU offers. A server announces one as a rectangle in a
# framebuffer update.
PSEUDO_QEMU_EXT_KEY_EVENT = -258
PSEUDO_EXTENDED_MOUSE_BUTTONS = -316

# Live client sockets, so the control port can drop them. A restart of QEMU
# looks exactly like this from the browser, and it is the case the console has
# to come back from (https://github.com/maglo/qemu-lab-manager/issues/54).
live = set()
live_lock = threading.Lock()

def drop_all():
    with live_lock:
        conns, live_now = list(live), len(live)
        live.clear()
    for conn in conns:
        try:
            conn.shutdown(socket.SHUT_RDWR)
        except OSError:
            pass
        conn.close()
    return live_now

def control(port):
    """A line on this port drops every console connection."""
    s = socket.socket()
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind(("127.0.0.1", port))
    s.listen(4)
    print(f"[fakevnc] control on 127.0.0.1:{port}", flush=True)
    while True:
        conn, _ = s.accept()
        dropped = drop_all()
        print(f"[fakevnc] dropped {dropped} connection(s) on request", flush=True)
        try:
            conn.sendall(f"dropped {dropped}\n".encode())
        except OSError:
            pass
        conn.close()

def pixel_format():
    # 32bpp, depth 24, little-endian, true colour, 8 bits each, shifts 16/8/0
    return struct.pack(">BBBBHHHBBB3x", 32, 24, 0, 1, 255, 255, 255, 16, 8, 0)

def framebuffer(tick):
    # A couple of bands so a screenshot shows something recognisable.
    rows = []
    for y in range(H):
        if y < H // 3:
            px = struct.pack("<I", 0x00203040)
        elif y < 2 * H // 3:
            px = struct.pack("<I", 0x00 << 24 | (0x30 + (tick * 7) % 0x80) << 16 | 0x60 << 8 | 0x90)
        else:
            px = struct.pack("<I", 0x00101418)
        rows.append(px * W)
    return b"".join(rows)

def serve(conn, addr):
    with live_lock:
        live.add(conn)
    try:
        conn.sendall(b"RFB 003.008\n")
        client_version = recvn(conn, 12)
        if not client_version:
            return
        conn.sendall(struct.pack(">BB", 1, 1))          # one security type: None
        chosen = recvn(conn, 1)
        conn.sendall(struct.pack(">I", 0))              # SecurityResult: OK
        recvn(conn, 1)                                  # ClientInit shared flag
        conn.sendall(struct.pack(">HH", W, H) + pixel_format() +
                     struct.pack(">I", len(NAME)) + NAME)
        print(f"[fakevnc] handshake complete with {addr}", flush=True)

        tick = 0
        offered = False
        while True:
            head = recvn(conn, 1)
            if not head:
                return
            msg = head[0]
            if msg == 0:      # SetPixelFormat
                recvn(conn, 19)
            elif msg == 2:    # SetEncodings
                pad_count = recvn(conn, 3)
                n = struct.unpack(">H", pad_count[1:3])[0]
                recvn(conn, 4 * n)
            elif msg == 3:    # FramebufferUpdateRequest
                recvn(conn, 9)
                tick += 1
                body = framebuffer(tick)
                rects = struct.pack(">HHHHi", 0, 0, W, H, 0) + body
                count = 1
                if not offered:
                    # Announce what this server supports, as QEMU does, before
                    # the first picture.
                    offered = True
                    count = 3
                    rects = (struct.pack(">HHHHi", 0, 0, 0, 0,
                                         PSEUDO_QEMU_EXT_KEY_EVENT) +
                             struct.pack(">HHHHi", 0, 0, 0, 0,
                                         PSEUDO_EXTENDED_MOUSE_BUTTONS) +
                             rects)
                conn.sendall(struct.pack(">BxH", 0, count) + rects)
            elif msg == 4:    # KeyEvent -- must never arrive without a lease
                recvn(conn, 7)
                print("[fakevnc] INPUT KeyEvent received", flush=True)
            elif msg == 5:    # PointerEvent, six bytes or seven
                mask = recvn(conn, 1)
                recvn(conn, 4)
                if mask and mask[0] & 0x80:
                    recvn(conn, 1)
                print("[fakevnc] INPUT PointerEvent received", flush=True)
            elif msg == 6:    # ClientCutText
                rest = recvn(conn, 7)
                # The length is signed: the extended clipboard writes it
                # negative, and its magnitude is what follows.
                n = abs(struct.unpack(">i", rest[3:7])[0])
                recvn(conn, n)
                print("[fakevnc] INPUT ClientCutText received", flush=True)
            elif msg == 255:  # QEMU client message
                sub = recvn(conn, 1)
                if not sub or sub[0] != 0:
                    print(f"[fakevnc] unknown QEMU sub-message {sub}", flush=True)
                    return
                recvn(conn, 10)
                print("[fakevnc] INPUT QEMUExtendedKeyEvent received", flush=True)
            else:
                print(f"[fakevnc] unknown client message {msg}", flush=True)
                return
    except (ConnectionError, OSError) as e:
        print(f"[fakevnc] {addr} gone: {e}", flush=True)
    finally:
        with live_lock:
            live.discard(conn)
        conn.close()

def recvn(conn, n):
    buf = b""
    while len(buf) < n:
        chunk = conn.recv(n - len(buf))
        if not chunk:
            return None
        buf += chunk
    return buf

if __name__ == "__main__":
    port = int(sys.argv[1])
    threading.Thread(target=control, args=(port + 1,), daemon=True).start()
    s = socket.socket()
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind(("127.0.0.1", port))
    s.listen(8)
    print(f"[fakevnc] listening on 127.0.0.1:{port}", flush=True)
    while True:
        conn, addr = s.accept()
        threading.Thread(target=serve, args=(conn, addr), daemon=True).start()
