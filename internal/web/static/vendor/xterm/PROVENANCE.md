# Vendored xterm.js

    Upstream:  https://github.com/xtermjs/xterm.js
    Package:   @xterm/xterm 5.5.0 (npm)
    Files:     lib/xterm.js -> xterm.js, css/xterm.css
    Licence:   MIT (LICENSE)

## Why a terminal emulator at all

The serial channel is a byte stream with no framing, and boot output is full
of ANSI escapes, carriage returns and cursor moves. Appending it to a `<pre>`
renders a progress bar as thousands of lines and a coloured kernel log as
mojibake, which defeats the point of having the scrollback.

## Why the npm bundle

`@xterm/xterm` ships `lib/xterm.js` as a UMD bundle, so a plain `<script>`
tag defines `window.Terminal` with no bundler and no module plumbing. Unlike
noVNC, there is nothing to gain from taking the repository source here: it is
TypeScript and would need compiling.

## Updating

    npm pack @xterm/xterm@X.Y.Z
    tar xzf xterm-xterm-X.Y.Z.tgz
    cp package/lib/xterm.js package/css/xterm.css package/LICENSE .
    sed -i '/^\/\/# sourceMappingURL=/d' xterm.js

Then update the version above. The source map is deliberately not vendored;
it roughly doubles the size and is of no use in a lab. The last step removes
the comment that points at it, because a browser that follows the comment
asks for a file labview does not serve and reports the miss as a parse error
on the bundle. A test in `internal/web` fails if the comment comes back.
