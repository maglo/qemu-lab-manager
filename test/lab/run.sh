#!/usr/bin/env bash
#
# Bring up a fake QEMU lab, drive labview in a real browser against it, tear
# it all down. Exits non-zero if any check fails.
#
#   test/lab/run.sh
#
# Nothing here talks to a real hypervisor. The point is to exercise the parts
# that unit tests cannot: that noVNC completes an RFB handshake through the
# console proxy, that scrollback reaches a browser before live bytes, that
# status frames stay out of the byte stream, and that the write lease
# actually gates the keyboard.
set -uo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$here/../.." && pwd)

# These two are baked into the machine files, so they are constants rather
# than knobs: overriding one without editing the fixture would just produce a
# lab whose machines point at nothing.
LAB_DIR=/tmp/labview-lab
VNC_PORT=15901

PORT=${PORT:-18080}
PROXY_PORT=${PROXY_PORT:-18081}
work=$(mktemp -d)
export SHOTS_DIR=${SHOTS_DIR:-$work/shots}

pids=()
cleanup() {
  local code=$?
  for pid in "${pids[@]:-}"; do
    [ -n "$pid" ] && kill "$pid" 2>/dev/null
  done
  if [ $code -ne 0 ]; then
    echo
    echo "=== labview log ==="; tail -40 "$work/labview.log" 2>/dev/null
    echo "=== proxy log ===";   tail -20 "$work/proxy.log" 2>/dev/null
    echo "=== fake vnc log ==="; tail -20 "$work/vnc.log" 2>/dev/null
  fi
  rm -rf "$LAB_DIR"
  return $code
}
trap cleanup EXIT

echo "== building labview"
( cd "$root" && go build -o "$work/labview" ./cmd/labview ) || exit 1

echo "== starting the fake lab in $LAB_DIR"
mkdir -p "$LAB_DIR"
python3 "$here/fakevnc.py" "$VNC_PORT"                  >"$work/vnc.log" 2>&1 &
pids+=($!)
python3 "$here/fakeserial.py" "$LAB_DIR/el9-build.sock"  >"$work/serial1.log" 2>&1 &
pids+=($!)
python3 "$here/fakeserial.py" "$LAB_DIR/serial-only.sock" >"$work/serial2.log" 2>&1 &
pids+=($!)
sleep 1

# The fixture is copied, because the driver adds a machine file and deletes
# it again to check that labview follows the directory.
export INVENTORY_DIR=$work/inventory.d
cp -r "$here/machines" "$INVENTORY_DIR"

echo "== starting labview on 127.0.0.1:$PORT"
"$work/labview" \
  -listen "127.0.0.1:$PORT" \
  -inventory "$INVENTORY_DIR" \
  -host-access fake \
  -recordings-dir "$work/recordings" \
  >"$work/labview.log" 2>&1 &
pids+=($!)

echo "== starting the identity-asserting proxy on 127.0.0.1:$PROXY_PORT"
node "$here/proxy.js" >"$work/proxy.log" 2>&1 &
pids+=($!)

# Wait for both to answer rather than sleeping and hoping.
for name in "labview:$PORT" "proxy:$PROXY_PORT"; do
  label=${name%%:*}; port=${name##*:}
  for _ in $(seq 1 40); do
    if curl -fs -o /dev/null "http://127.0.0.1:$port/healthz"; then
      echo "   $label up"; break
    fi
    sleep 0.25
  done
  curl -fsS -o /dev/null "http://127.0.0.1:$port/healthz" || {
    echo "$label never became healthy"; exit 1
  }
done

echo "== driving a browser"
node "$here/drive.js"
code=$?

echo
if [ $code -eq 0 ]; then
  echo "browser checks passed; screenshots in $SHOTS_DIR"
else
  echo "browser checks FAILED; screenshots in $SHOTS_DIR"
fi
exit $code
