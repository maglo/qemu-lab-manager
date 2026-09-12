# Vendored noVNC

    Upstream:  https://github.com/novnc/noVNC
    Version:   v1.6.0
    Commit:    a8dfd6a3ea3c74244f5ebdaa5a7f1023007a7820 (tag v1.6.0)
    Licence:   MPL 2.0 (LICENSE.txt, AUTHORS)
    Bundled:   vendor/pako, MIT (vendor/pako/LICENSE)

## Why this is vendored rather than fetched

labview binds loopback on a hypervisor in a lab that is not expected to
reach the internet, so the browser cannot be sent to a CDN. Vendoring also
means building labview needs nothing but a Go toolchain.

## Why the repository and not the npm package

`@novnc/novnc` on npm ships `lib/`, which is Babel-transpiled CommonJS and
will not load in a browser without a bundler. The repository ships `core/`
as native ES modules, which the browser loads directly. Keeping it as
readable source rather than a bundled artifact also means this directory can
be diffed against upstream.

## Layout

`core/` imports `../vendor/pako/...`, so both directories are kept with
their relative positions intact. Nothing here is modified; upstream's test
and benchmark directories are the only things dropped.

## Updating

    git clone --depth 1 --branch vX.Y.Z https://github.com/novnc/noVNC /tmp/novnc
    rm -rf core vendor
    cp -r /tmp/novnc/core .
    mkdir -p vendor && cp -r /tmp/novnc/vendor/pako vendor/
    cp /tmp/novnc/LICENSE.txt /tmp/novnc/AUTHORS .
    rm -rf vendor/pako/test vendor/pako/benchmark vendor/pako/dist

Then update the version and commit above, and check the RFB constructor
signature in `docs/API.md` against `static/app/console.js`.
