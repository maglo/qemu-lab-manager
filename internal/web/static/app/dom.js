// Small DOM helpers. No framework: the UI is three states and six tabs, and
// a dependency would outweigh it.

export const $ = (sel, root = document) => root.querySelector(sel);
export const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

export function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === null || v === undefined || v === false) continue;
    if (k === 'class') node.className = v;
    else if (k === 'text') node.textContent = v;
    else if (k.startsWith('on') && typeof v === 'function') {
      node.addEventListener(k.slice(2).toLowerCase(), v);
    } else if (v === true) node.setAttribute(k, '');
    else node.setAttribute(k, String(v));
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined || c === false) continue;
    node.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return node;
}

export function clear(node) {
  while (node.firstChild) node.removeChild(node.firstChild);
}

let toastTimer = null;

export function toast(message, kind = '') {
  const node = $('#toast');
  node.textContent = message;
  node.className = `toast ${kind}`.trim();
  node.hidden = false;
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { node.hidden = true; }, 4000);
}

// A copyable block.
//
// The copied text is the plain, unwrapped original, never what the browser
// happened to render: a command line copied with its visual wrapping is worse
// than useless (design section 8).
export function copyBlock(text, { wrapped = false } = {}) {
  const pre = el('pre', { class: wrapped ? 'code wrapped' : 'code', text });
  const button = el('button', {
    class: 'ghost',
    type: 'button',
    title: 'Copy',
    onclick: async (ev) => {
      ev.stopPropagation();
      try {
        await navigator.clipboard.writeText(text);
        toast('Copied', 'good');
      } catch {
        // Clipboard access needs a secure context, which plain http on a
        // hypervisor is not. Fall back to a selection the user can copy.
        const range = document.createRange();
        range.selectNodeContents(pre);
        const sel = window.getSelection();
        sel.removeAllRanges();
        sel.addRange(range);
        toast('Selected -- press Ctrl+C to copy');
      }
    },
  }, 'copy');
  return el('div', { class: 'copywrap' }, pre, button);
}

export function block(title, ...children) {
  return el('div', { class: 'block' }, el('h2', { text: title }), ...children);
}

export function kvTable(rows) {
  return el('table', { class: 'kv' },
    el('tbody', {}, rows
      .filter(([, v]) => v !== null && v !== undefined && v !== '')
      .map(([k, v]) => el('tr', {},
        el('th', { text: k }),
        el('td', {}, v instanceof Node ? v : String(v))))));
}

export function bytes(n) {
  if (!n) return '0 B';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return `${v < 10 && i > 0 ? v.toFixed(1) : Math.round(v)} ${units[i]}`;
}

export function when(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  if (isNaN(d.getTime()) || d.getTime() === 0) return '';
  return d.toLocaleString();
}

export function ago(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  if (isNaN(d.getTime()) || d.getTime() === 0) return '';
  const secs = Math.round((Date.now() - d.getTime()) / 1000);
  if (secs < 60) return `${secs}s ago`;
  if (secs < 3600) return `${Math.round(secs / 60)}m ago`;
  if (secs < 86400) return `${Math.round(secs / 3600)}h ago`;
  return `${Math.round(secs / 86400)}d ago`;
}
