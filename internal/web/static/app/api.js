// The JSON surface of design section 5, and nothing more.
//
// Everything the UI displays is fetched through here, which is the same path
// a test harness uses: the UI is one consumer of the API, not a privileged
// one (design section 13).

async function json(path, options) {
  const res = await fetch(path, {
    ...options,
    headers: { 'Content-Type': 'application/json', ...(options?.headers || {}) },
  });
  let body = null;
  try {
    body = await res.json();
  } catch {
    // A non-JSON body means something in front of labview answered.
  }
  if (!res.ok) {
    const err = new Error((body && body.error) || `${res.status} ${res.statusText}`);
    err.status = res.status;
    // A refusal carries who holds the lease, so the caller can say so
    // without a second request (design section 4).
    err.holder = body && body.holder;
    err.expires = body && body.expires;
    throw err;
  }
  return body;
}

export const api = {
  config: () => json('/api/config'),
  machines: () => json('/api/machines'),
  machine: (id) => json(`/api/machines/${encodeURIComponent(id)}`),
  logs: (id, lines = 200) =>
    json(`/api/machines/${encodeURIComponent(id)}/logs?lines=${lines}`),
  recordings: (id) => json(`/api/machines/${encodeURIComponent(id)}/recordings`),
  activity: (id) => json(`/api/machines/${encodeURIComponent(id)}/activity`),

  lease: (id, action) =>
    json(`/api/machines/${encodeURIComponent(id)}/lease`, {
      method: 'POST',
      body: JSON.stringify({ action }),
    }),

  power: (id, op) =>
    json(`/api/machines/${encodeURIComponent(id)}/power`, {
      method: 'POST',
      body: JSON.stringify({ op }),
    }),

  recordingURL: (id, name, inline = false) =>
    `/api/machines/${encodeURIComponent(id)}/recordings/${encodeURIComponent(name)}` +
    (inline ? '?inline=1' : ''),
};

// Websocket URLs. Same origin, so the scheme follows the page's: behind the
// proxy this is wss, and the origin check on the server compares Origin to
// the forwarded Host (design section 10).
export function wsURL(path) {
  const scheme = location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${scheme}//${location.host}${path}`;
}

export const ws = {
  console: (id, write) =>
    wsURL(`/ws/console/${encodeURIComponent(id)}${write ? '?write=1' : ''}`),
  serial: (id, write) =>
    wsURL(`/ws/serial/${encodeURIComponent(id)}${write ? '?write=1' : ''}`),
  logs: (id) => wsURL(`/ws/logs/${encodeURIComponent(id)}`),
};
