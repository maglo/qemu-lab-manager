# Changelog

Every release of labview, newest first. A pull request does not edit this
file. It adds a fragment to `changelogs/fragments/`, and a release compiles
the fragments. See `docs/design/changelog.md`.

## 0.1.0 -- 2026-09-13

### Release Summary

The first release of labview, the console wall of the QEMU lab. It shows every
machine of one hypervisor on one page, and it opens the framebuffer, the serial
line, the journal and the details of a machine without an SSH session on the
host. It is one Go binary, one inventory directory and no database.

### Major Changes

- api - serve everything the wall shows as JSON. The browser application is one
  consumer and a test harness is another, and neither gets a privileged path.
- console - proxy the framebuffer of a machine to the browser over a websocket.
  labview reads enough RFB to tell input traffic from viewing traffic, so a
  viewer without the write lease watches and does not type.
- container - publish the image to the registry of this repository. It is the
  binary on `scratch` and it runs as uid 65532
  (https://github.com/maglo/qemu-lab-manager/issues/14).
- deploy - ship the systemd unit, the polkit rule and the nginx configuration.
  labview runs as its own user, the polkit rule covers the units of the lab
  only, and the proxy holds the TLS, the SSO and the headers that labview
  depends on.
- host - read the unit, the command line, the disks, the network and the journal
  of a machine from the hypervisor, without libvirt and without an agent.
  `-host-access fake` invents the details so the wall can be worked on with no
  lab attached, and it is never a fallback.
- inventory - keep the entry that last loaded when a file does not load, and
  name the file in `/healthz`. A typo takes down one machine at most. The first
  load is strict, so labview does not start with a file it cannot read.
- inventory - read one YAML file for each machine from a directory, and take the
  file name as the id of the machine. labview watches the directory, so a new
  file, a changed file and a deleted file reach the wall at once, and it reads
  the directory again every `-inventory-rescan` as a backstop
  (https://github.com/maglo/qemu-lab-manager/issues/16).
- labview - show every machine of the lab on one wall. A tile gives the name,
  the liveness and the holder of the write lease. Open a machine to reach its
  console, its serial line, its journal, its details, its recordings and its
  activity.
- lease - give the keyboard to one person at a time. The proxy names the person
  in `X-Forwarded-User`, the lease expires, and the activity of a machine
  records who attached and who held control.
- power - start, stop and restart a machine through its systemd unit. The `unit`
  field of the inventory is an allowlist: a machine with no unit has no power
  operations, and labview does not guess a unit name from an id.
- serial - capture the output to asciicast v2, one file for each run of a
  machine, and rotate on a reconnect. Retention is by count, by age and by total
  size, with a ceiling for one file.
- serial - hold one connection to the serial port of each machine and fan it out
  to every subscriber. A subscriber gets the scrollback first, so a harness can
  open a machine that is switched off, start it, and watch it boot from the
  first line.
- serial - separate what the machine printed from what labview says about
  itself. A binary frame is the output of the machine, and a text frame is
  labview. A status message mixed into the output would land in the recording
  and in anything that greps the stream.

### Known Issues

- container - the logs tab and the disk sizes of the details tab do not work in
  the container, because the image holds no `journalctl` and no `qemu-img`. Both
  say what is missing.
- labview - one labview serves one hypervisor. There is no aggregation of
  several hypervisors yet.
- labview - the screenshot tile has no capture source. `-tile-mode` selects the
  renderer, and the renderer says what it waits for rather than inventing an
  endpoint.
- lease - a lease cannot be stolen. A lease that expires releases the keyboard,
  and there is no way to take it sooner.
