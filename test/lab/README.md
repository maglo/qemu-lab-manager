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
| `fakevnc.py` | RFB 3.8 server: handshake, one framebuffer, and it logs any input it receives — which is how "a viewer cannot type" is checked |
| `fakeserial.py` | A unix socket that prints a boot with ANSI colour, then echoes what is typed at it |
| `proxy.js` | Stands in for design section 10: terminates the browser's connection, asserts `X-Forwarded-User`, forwards the original `Host` |
| `machines/` | Four machine files: one with both channels, one serial-only, one switched off, one with neither |
| `drive.js` | The checks |

`run.sh` copies `machines/` to a temporary directory. The checks write a new
machine file there and delete it again, which is how "labview follows the
directory" is checked.

## The proxy is not incidental

It asserts the identity **on the websocket upgrade as well as on ordinary
requests**. That is the topology labview is deployed in, and getting it wrong
is silent: every console and serial socket arrives unidentified, no lease
matches the person holding it, and typing does nothing at all while the UI
insists you have control. Driving labview directly on `$PORT` instead of
through the proxy reproduces that, which is worth knowing when a check
suddenly fails on input delivery.

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
