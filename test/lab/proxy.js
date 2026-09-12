// A stand-in for the reverse proxy of design section 10: it terminates the
// browser's connection, asserts an identity, and forwards the original Host
// header so labview's websocket origin check still works.
//
// Injecting the header on the upgrade as well as on ordinary requests is the
// part that matters: a proxy that only sets it on plain HTTP leaves every
// websocket unidentified, and no lease would ever match.
const http = require('http');

const USER = process.env.LABVIEW_USER || 'alice@example.com';
const target = { host: '127.0.0.1', port: Number(process.env.PORT || 18080) };

function withIdentity(headers) {
  return { ...headers, 'x-forwarded-user': USER };
}

process.on('uncaughtException', (e) => console.error('[proxy] ignored:', e.code || e.message));

const server = http.createServer((req, res) => {
  const up = http.request(
    { ...target, path: req.url, method: req.method, headers: withIdentity(req.headers) },
    (ur) => { res.writeHead(ur.statusCode, ur.headers); ur.pipe(res); });
  up.on('error', () => { res.writeHead(502); res.end('proxy error'); });
  req.pipe(up);
});

server.on('upgrade', (req, socket, head) => {
  const up = http.request({ ...target, path: req.url, headers: withIdentity(req.headers) });
  // A console or serial socket ends whenever a tab closes, so a reset on
  // either side is routine. Unhandled, it takes the proxy down.
  socket.on('error', () => socket.destroy());

  up.on('upgrade', (ur, usocket, uhead) => {
    usocket.on('error', () => usocket.destroy());
    const lines = Object.entries(ur.headers).map(([k, v]) => `${k}: ${v}`);
    socket.write(`HTTP/1.1 101 Switching Protocols\r\n${lines.join('\r\n')}\r\n\r\n`);
    if (uhead && uhead.length) socket.write(uhead);
    usocket.pipe(socket).on('error', () => socket.destroy());
    socket.pipe(usocket).on('error', () => usocket.destroy());
  });
  up.on('response', (ur) => {
    // labview refused the upgrade (for example a machine with no console);
    // pass the refusal through so the browser can read it.
    const lines = Object.entries(ur.headers).map(([k, v]) => `${k}: ${v}`);
    socket.write(`HTTP/1.1 ${ur.statusCode} ${ur.statusMessage}\r\n${lines.join('\r\n')}\r\n\r\n`);
    ur.pipe(socket);
  });
  up.on('error', () => socket.destroy());
  up.end();
});

const listenPort = Number(process.env.PROXY_PORT || 18081);
server.listen(listenPort, '127.0.0.1', () =>
  console.log(`[proxy] 127.0.0.1:${listenPort} -> ${target.host}:${target.port}, asserting ${USER}`));
