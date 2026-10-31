// 读取 9224 上某标签页的 window.__probe，并按需截图（输出 PNG 像素尺寸）。
// 用法: node probe-read.mjs <urlSubstring> [outPng]
const PORT = 9224;
const urlSub = process.argv[2] || 'probe.html';
const outPng = process.argv[3] || '';
const ver = await (await fetch(`http://127.0.0.1:${PORT}/json/version`)).json();
const list = await (await fetch(`http://127.0.0.1:${PORT}/json/list`)).json();
let t = list.find(x => x.type === 'page' && (x.url || '').includes(urlSub));
const createIfMissing = !!process.env.WTX_CREATE;
let created = null;
if (!t && createIfMissing) {
  const full = urlSub.startsWith('http') ? urlSub : `http://127.0.0.1:8791/${urlSub}`;
  const mk = await (await fetch(`http://127.0.0.1:${PORT}/json/new?${encodeURIComponent(full)}`, { method: 'PUT' })).json();
  t = { id: mk.id, url: mk.url }; created = mk.id;
  await new Promise(r => setTimeout(r, 800));
}
if (!t) { console.log(JSON.stringify({ error: 'no target', list: list.map(x => ({type:x.type,url:x.url})) })); process.exit(2); }
const ws = new WebSocket(ver.webSocketDebuggerUrl);
const pending = new Map(); let seq = 0;
ws.addEventListener('message', (e) => { const m = JSON.parse(e.data);
  if (m.id && pending.has(m.id)) { const p = pending.get(m.id); pending.delete(m.id); m.error ? p.reject(new Error(JSON.stringify(m.error))) : p.resolve(m.result); } });
const send = (method, params = {}, sessionId) => new Promise((res, rej) => { const id = ++seq; pending.set(id, {resolve:res, reject:rej}); ws.send(JSON.stringify({id, method, params, sessionId})); });
await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });
const { sessionId } = await send('Target.attachToTarget', { targetId: t.id, flatten: true });
await send('Runtime.enable', {}, sessionId);
const ev = await send('Runtime.evaluate', { expression: 'JSON.stringify({probe: window.__probe||null, ready: !! window.__ready, href: location.href, title: document.title})', returnByValue: true }, sessionId);
const out = { created,
  target: { id: t.id, url: t.url }, page: JSON.parse(ev.result.value) };
if (outPng) {
  const shot = await send('Page.captureScreenshot', { format: 'png', fromSurface: true }, sessionId);
  const buf = Buffer.from(shot.data, 'base64');
  const fs = await import('node:fs');
  fs.writeFileSync(outPng, buf);
  const w = buf.readUInt32BE(16), h = buf.readUInt32BE(20);
  out.screenshot = { path: outPng, pngW: w, pngH: h, bytes: buf.length };
}
console.log(JSON.stringify(out, null, 1));
ws.close();
