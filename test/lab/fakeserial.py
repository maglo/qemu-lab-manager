#!/usr/bin/env python3
"""A unix socket that prints a plausible boot, so the serial channel has
something real to carry."""
import os, socket, sys, threading, time

BOOT = [
    "[    0.000000] Linux version 5.14.0-362.el9.x86_64 (mockbuild@x86-64) \r\n",
    "[    0.000000] Command line: BOOT_IMAGE=/vmlinuz root=/dev/mapper/rhel-root\r\n",
    "[    0.004000] BIOS-provided physical RAM map:\r\n",
    "[    0.120000] ACPI: Early table checksum verification disabled\r\n",
    "[    1.884000] systemd[1]: Detected virtualization kvm.\r\n",
    "\x1b[0;32m  OK  \x1b[0m Started \x1b[1mJournal Service\x1b[0m.\r\n",
    "\x1b[0;32m  OK  \x1b[0m Reached target \x1b[1mBasic System\x1b[0m.\r\n",
    "\x1b[0;31mFAILED\x1b[0m Failed to start \x1b[1mfoo.service\x1b[0m.\r\n",
    "\r\nAlmaLinux 9.3 (Shamrock Pampas Cat)\r\nKernel 5.14.0 on an x86_64\r\n\r\n",
    "el9-build login: ",
]

def serve(path):
    if os.path.exists(path):
        os.unlink(path)
    s = socket.socket(socket.AF_UNIX)
    s.bind(path)
    s.listen(4)
    print(f"[fakeserial] listening on {path}", flush=True)
    while True:
        conn, _ = s.accept()
        threading.Thread(target=session, args=(conn,), daemon=True).start()

def session(conn):
    try:
        for line in BOOT:
            conn.sendall(line.encode())
            time.sleep(0.12)
        # Echo anything typed, so input is visibly delivered or visibly not.
        while True:
            data = conn.recv(1024)
            if not data:
                return
            print(f"[fakeserial] INPUT {data!r}", flush=True)
            conn.sendall(data)
    except (ConnectionError, OSError):
        pass
    finally:
        conn.close()

if __name__ == "__main__":
    serve(sys.argv[1])
