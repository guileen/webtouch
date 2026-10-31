// T-3 真机复证 · 证据①（页面侧）：对真实页面执行 pkg/browser/find/element.go 的
// elementAtPointJS（从构建产物的源码逐字抽取，X/Y 按 Go 侧顺序代入），核对 {sel,tag,text}。
// 只读：不派发输入、不改页面。
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const PORT = Number(process.env.WTX_PORT || 9224);
const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const SRC = path.join(REPO_ROOT, 'pkg/browser/find/element.go');
const TARGET_URL = process.argv[2] || 'https://www.iana.org/help/example-domains';

const src = fs.readFileSync(SRC, 'utf8');
const m = src.match(/const elementAtPointJS = `([\s\S]*?)`\n/);
if (!m) throw new Error('未能在源码中定位 elementAtPointJS 常量');
const ELEMENT_AT_POINT_JS = m[1];
const atPoint = (x, y) => ELEMENT_AT_POINT_JS.replaceAll('X', String(x)).replaceAll('Y', String(y));

const ver = await (await fetch(`http://127.0.0.1:${PORT}/json/version`)).json();
const ws = new WebSocket(ver.webSocketDebuggerUrl);
const pending = new Map(); const events = []; let seq = 0;
ws.addEventListener('message', (e) => { const msg = JSON.parse(e.data);
  if (msg.id && pending.has(msg.id)) { const p = pending.get(msg.id); pending.delete(msg.id); msg.error ? p.reject(new Error(JSON.stringify(msg.error))) : p.resolve(msg.result); }
  else if (msg.method) events.push(msg); });
const send = (method, params = {}, sessionId) => new Promise((resolve, reject) => { const id = ++seq; pending.set(id, {resolve, reject}); ws.send(JSON.stringify({id, method, params, ...(sessionId?{sessionId}:{})})); });
const waitEvent = (method, sessionId, ms = 30000) => new Promise((resolve, reject) => { const t0 = Date.now();
  const tick = () => { const i = events.findIndex((e) => e.method === method && (!sessionId || e.sessionId === sessionId));
    if (i >= 0) return resolve(events.splice(i, 1)[0]); if (Date.now() - t0 > ms) return reject(new Error(`等待 ${method} 超时`)); setTimeout(tick, 50); }; tick(); });
await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });

const { targetId } = await send('Target.createTarget', { url: 'about:blank' });
const { sessionId } = await send('Target.attachToTarget', { targetId, flatten: true });
await send('Page.enable', {}, sessionId);
await send('Emulation.setDeviceMetricsOverride', { width: 1440, height: 1200, deviceScaleFactor: 1, mobile: false }, sessionId);
const evaluate = async (expression) => { const r = await send('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true }, sessionId);
  if (r.exceptionDetails) throw new Error('evaluate 异常: ' + JSON.stringify(r.exceptionDetails)); return r.result.value; };

const load = waitEvent('Page.loadEventFired', sessionId);
await send('Page.navigate', { url: TARGET_URL }, sessionId);
await load;
for (let i = 0; i < 60; i++) { if (await evaluate('document.readyState === "complete" && !!document.querySelector("h1")')) break; await new Promise((r) => setTimeout(r, 250)); }

const centerOf = async (selector) => { const r = await evaluate(`(() => {
  const el = document.querySelector(${JSON.stringify(selector)}); if (!el) return null;
  const b = el.getBoundingClientRect(); if (b.width < 2 || b.height < 2) return null;
  return JSON.stringify({ x: Math.round(b.left + b.width/2), y: Math.round(b.top + b.height/2),
    innerText: (el.innerText||'').replace(/\\s+/g,' ').trim(), tag: el.tagName.toLowerCase(),
    rect: [Math.round(b.left),Math.round(b.top),Math.round(b.width),Math.round(b.height)] }); })()`);
  return r ? JSON.parse(r) : null; };

const crossCheck = async (x, y, sel, text) => JSON.parse(await evaluate(`(() => {
  const at = document.elementFromPoint(${x}, ${y});
  const bySel = ${sel ? `document.querySelector(${JSON.stringify(sel)})` : 'null'};
  const norm = (s) => s == null ? '' : String(s).replace(/\\s+/g,' ').trim();
  let level = ''; let n = at;
  for (let up = 0; n && up <= 3; up++) { if (norm(n.innerText) === ${JSON.stringify(text)}) { level = 'self+ancestor' + up; break; } n = n.parentElement; }
  return JSON.stringify({ selResolvesToSameNode: !!at && !!bySel && at === bySel,
    textLevel: level, pointTag: at ? at.tagName.toLowerCase() : 'null',
    bySelTag: bySel ? bySel.tagName.toLowerCase() : 'null' }); })()`));

const probe = async (label, x, y, note) => { const raw = await evaluate(atPoint(x, y));
  return { label, x, y, note, raw, textRunes: [...raw.text].length, crossCheck: await crossCheck(x, y, raw.sel, raw.text) }; };

const out = { browser: ver.Browser, targetUrl: await evaluate('location.href'), title: await evaluate('document.title'),
  viewport: await evaluate('JSON.stringify([innerWidth, innerHeight, document.documentElement.scrollHeight])'), cases: [] };

// A：标题 h1（带 id，选择器应被 id 截断）
const h1 = await centerOf('h1#example-domains');
out.cases.push(await probe('A: h1#example-domains 中心', h1.x, h1.y, `ground truth innerText=${JSON.stringify(h1.innerText)} rect=${h1.rect}`));

// B：导航链接（内联文本元素）
const nav = await centerOf('#header .navigation a');
out.cases.push(await probe('B: 导航链接 <a> 中心', nav.x, nav.y, `ground truth innerText=${JSON.stringify(nav.innerText)} rect=${nav.rect}`));

// C：图标型 <a>（自身 innerText 为空 ⇒ 触发「上溯 ≤3 层祖先」）
const logo = await centerOf('#logo > a');
if (logo) out.cases.push(await probe('C: 图标链接 <a><img></a>（空文本 ⇒ 上溯祖先）', logo.x, logo.y, `ground truth a innerText=${JSON.stringify(logo.innerText)} rect=${logo.rect}`));

// D：空白/无命中（期望零值：命中 body/documentElement，或视口外返回 null）
const blank = await evaluate(`(() => {
  const isBlank = (el) => !el || el === document.body || el === document.documentElement;
  for (let y = innerHeight - 2; y > 0; y -= 7) for (let x = 2; x < innerWidth; x += 23) {
    const el = document.elementFromPoint(x, y); if (isBlank(el)) return JSON.stringify({ x, y, tag: el ? el.tagName.toLowerCase() : 'null' });
  }
  return JSON.stringify({ x: -1, y: -1, tag: 'outside-viewport(null)' }); })()`);
const bl = JSON.parse(blank);
out.cases.push(await probe('D: 空白区（命中 body/html ⇒ 期望零值）', bl.x, bl.y, `elementFromPoint=${bl.tag}`));

// E：长文本元素（>120 rune）：JS 原样返回全文，按 rune 截断由 Go 侧 apply（见 element.go::truncateRunes）
const para = await centerOf('.help-article p');
out.cases.push(await probe('E: 长段落 <p>（>120 rune，Go 侧截断）', para.x, para.y, `ground truth innerText 长度 rune=${[...para.innerText].length}`));

out.constant = { source: SRC, chars: ELEMENT_AT_POINT_JS.length, stillHasXYPlaceholder: /elementFromPoint\(X, Y\)/.test(ELEMENT_AT_POINT_JS) };
await send('Target.closeTarget', { targetId });
console.log(JSON.stringify(out, null, 2));
ws.close();
