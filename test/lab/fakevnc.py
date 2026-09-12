#!/usr/bin/env python3
"""A minimal RFB 3.8 server: handshake, one solid framebuffer, nothing else.

Enough to prove that labview's console proxy, its RFB input filter and the
vendored noVNC all agree on the protocol.
"""
import socket, struct, sys, threading, time

W, H = 640, 480
NAME = b"fake-el9-build"

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
                conn.sendall(struct.pack(">BxH", 0, 1) +
                             struct.pack(">HHHHi", 0, 0, W, H, 0) + body)
            elif msg == 4:    # KeyEvent -- must never arrive without a lease
                recvn(conn, 7)
                print("[fakevnc] INPUT KeyEvent received", flush=True)
            elif msg == 5:    # PointerEvent
                recvn(conn, 5)
                print("[fakevnc] INPUT PointerEvent received", flush=True)
            elif msg == 6:    # ClientCutText
                rest = recvn(conn, 7)
                n = struct.unpack(">I", rest[3:7])[0]
                recvn(conn, n)
                print("[fakevnc] INPUT ClientCutText received", flush=True)
            else:
                print(f"[fakevnc] unknown client message {msg}", flush=True)
                return
    except (ConnectionError, OSError) as e:
        print(f"[fakevnc] {addr} gone: {e}", flush=True)
    finally:
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
    s = socket.socket()
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.bind(("127.0.0.1", port))
    s.listen(8)
    print(f"[fakevnc] listening on 127.0.0.1:{port}", flush=True)
    while True:
        conn, addr = s.accept()
        threading.Thread(target=serve, args=(conn, addr), daemon=True).start()
