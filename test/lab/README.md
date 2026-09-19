# The fake lab

A QEMU lab that isn't one: a minimal RFB server, unix sockets that print a
plausible boot, and a reverse proxy that asserts an identity. Enough for
labview to be driven in a real browser with no hypervisor anywhere.

    npm install                        # once, in this directory
    npx playwright install chromium    # once
    test/lab/run.sh

`run.sh` installs nothing. Exits non-zero if any check fails. Screenshots land
in a temp directory the run prints; set `SHOTS_DIR` to put them somewhere you
choose. A machine that has a browser already can skip the download: point
`CHROMIUM_PATH` at the executable.

`01-wall.png` is the picture in the top level `README.md`. A change to the
wall makes that picture wrong, so copy the new shot to `docs/images/wall.png`
in the same branch.

## Why this exists

Unit tests cover the broker, the lease, the RFB framing and the API. They
cannot cover the parts where labview meets a browser, and that is where the
interesting bugs were: a CSS cascade slip that painted four tab panels at
once, a framebuffer canvas that swallowed the click meant to open its tile,
and a capture that was empty on disk for as long as the machine kept running.
Each passed every unit test.

So this drives the real thing and asserts on what a developer would see.

## What each piece is

| File | Role |
|---|---|
| `fakevnc.py` | RFB 3.8 server: handshake, one framebuffer, the two pseudo-encodings QEMU offers, and it logs any input it receives — which is how "a viewer cannot type" is checked |
| `fakeserial.py` | A unix socket that prints a boot with ANSI colour, then echoes what is typed at it |
| `fakeqmp.py` | A unix socket that answers QMP: `send-key`, and `screendump` that writes a real PNG |
| `proxy.js` | Stands in for design section 10: terminates the browser's connection, asserts `X-Forwarded-User`, forwards the original `Host` |
| `machines/` | Four machine files: one with all three channels, one serial-only, one switched off, one with neither |
| `drive.js` | The checks |

`run.sh` copies `machines/` to a temporary directory. The checks write a new
machine file there and delete it again, which is how "labview follows the
directory" is checked.

## The fake VNC server offers what QEMU offers

`fakevnc.py` announces the QEMU extended key event and the extended mouse
buttons, because noVNC sends different messages once a server offers them: a
key becomes client message 255 instead of 4, and a pointer event grows a
seventh byte. A fake that offers neither exercises a client the lab never
runs, and labview's input filter closed the console on the first key press of
every real session while every check here passed.

The console keyboard is checked through the lease clock. A successful key is
not logged, so `drive.js` samples the lease expiry, presses one key over the
framebuffer and samples again: input that reaches the server pushes the expiry
out. The click that focuses the framebuffer comes before the first sample,
because a pointer event is input as well and would renew the lease on its own.

The second check is that the canvas is still there afterwards. noVNC removes
it when the connection closes, and the connection carries the framebuffer
update requests, so a console that survives a key press is a console that is
still drawing.

`fakevnc.py` also takes one line on the port above the one it serves, and
closes every console connection when it gets one. That is what a restart of
QEMU looks like from the browser, and it is how "the console comes back
without a reload" is checked.

## The fake QMP socket

`fakeqmp.py` writes an asynchronous `event` line before every reply. QEMU
does that whenever it feels like it, and a client that takes the next line
for its reply reads the wrong one for every command after it. The symptom
looks exactly like `screendump` returning before the file exists, so the fake
produces the case on every command rather than leaving it to chance.

It writes a real PNG, of a different colour each time. That is what lets a
check see a screenshot tile take a new frame instead of keeping the first
one.

## Two labviews

`run.sh` starts a second labview on `$SHOT_PORT`, on the same inventory, with
`-tile-mode=screenshot`. The mode is a flag, so a second process is the only
way to drive both walls in one run.

The driver opens that one directly instead of through the proxy, so it is
unidentified and holds no lease. That is the point: a screenshot is a read,
and read is free.

## The proxy is not incidental

It asserts the identity **on the websocket upgrade as well as on ordinary
requests**. That is the topology labview is deployed in, and getting it wrong
is silent: every console and serial socket arrives unidentified, no lease
matches the person holding it, and typing does nothing at all while the UI
insists you have control. Driving labview directly on `$PORT` instead of
through the proxy reproduces that, which is worth knowing when a check
suddenly fails on input delivery.

## The console error policy

`drive.js` fails the run on any console error it does not list as expected.
That is what caught a content-security-policy breakage, and it is why the
checks for 404 answers are worth reading twice: a path that the application
asks for itself must exist, and the browser asks for `/favicon.ico` unless the
page names an icon.

## Adding a check

`drive.js` uses one helper worth knowing about. `newClient` wraps a websocket
with a reader goroutine-equivalent, because coder/websocket closes the
connection when a read's context is cancelled — so a test cannot "read for
200ms and carry on". It reads continuously, as a browser does, and assertions
look at what has accumulated.

And assert on what is painted, not only on ARIA state: `onlyPanelVisible`
exists because the first version of these checks passed straight through four
panels rendering on top of each other by only ever asking which tab was
marked selected.
