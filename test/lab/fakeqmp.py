#!/usr/bin/env python3
"""A unix socket that answers QMP, so the control channel has something real
to talk to.

It writes an asynchronous event line before every reply. QEMU does that
whenever it feels like it, and a client that takes the next line for its reply
reads the wrong one for every command after it."""
import json, os, socket, struct, sys, threading, zlib

GREETING = {"QMP": {"version": {"qemu": {"major": 10, "minor": 1, "micro": 0},
                                "package": "fake"}, "capabilities": []}}

def png(width, height, rgb):
    """The smallest valid PNG of one colour, so no image library is needed."""
    raw = b"".join(b"\x00" + bytes(rgb) * width for _ in range(height))
    def chunk(kind, data):
        return (struct.pack(">I", len(data)) + kind + data
                + struct.pack(">I", zlib.crc32(kind + data) & 0xffffffff))
    header = struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0)
    return (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", header)
            + chunk(b"IDAT", zlib.compress(raw)) + chunk(b"IEND", b""))

def serve(path):
    if os.path.exists(path):
        os.unlink(path)
    s = socket.socket(socket.AF_UNIX)
    s.bind(path)
    s.listen(4)
    print(f"[fakeqmp] listening on {path}", flush=True)
    while True:
        conn, _ = s.accept()
        threading.Thread(target=session, args=(conn,), daemon=True).start()

def session(conn):
    frames = 0
    try:
        send(conn, GREETING)
        for line in conn.makefile("rb"):
            try:
                request = json.loads(line)
            except ValueError:
                continue
            command = request.get("execute")
            arguments = request.get("arguments") or {}
            send(conn, {"event": "RTC_CHANGE",
                        "timestamp": {"seconds": 1, "microseconds": 0},
                        "data": {"offset": 1}})

            if command in ("qmp_capabilities", "send-key"):
                if command == "send-key":
                    keys = [k.get("data") for k in arguments.get("keys", [])]
                    print(f"[fakeqmp] INPUT {'+'.join(keys)} "
                          f"hold={arguments.get('hold-time')}", flush=True)
                send(conn, {"return": {}})
            elif command == "screendump":
                if arguments.get("format") != "png":
                    send(conn, {"error": {"class": "GenericError",
                                          "desc": "this lab wants format png"}})
                    continue
                # A different colour each time, so a check can see the tile
                # take a new frame rather than keep the first one.
                frames += 1
                colour = (32 * (frames % 8), 90, 160)
                with open(arguments["filename"], "wb") as f:
                    f.write(png(160, 120, colour))
                send(conn, {"return": {}})
            else:
                send(conn, {"error": {"class": "CommandNotFound",
                                      "desc": f"unknown command {command}"}})
    except (ConnectionError, OSError):
        pass
    finally:
        conn.close()

def send(conn, message):
    conn.sendall(json.dumps(message).encode() + b"\n")

if __name__ == "__main__":
    serve(sys.argv[1])
