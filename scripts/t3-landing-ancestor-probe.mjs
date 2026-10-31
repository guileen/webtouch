// T-3 落点证据「上溯可信边界」真机探针（2026-09-30 订正后）：
// 逐字取自 pkg/browser/find/element.go 的 elementAtPointJS，在自建无头 Chrome 的
// 真实渲染页面上对六种落点形态取证，并给出**旧口径**（为空即取首个非空祖先）对照。
// 只读：只建 DOM 与读文本，不派发任何输入事件。
// 用法：node scripts/t3-landing-ancestor-probe.mjs （前置：自建 Chrome --remote-debugging-port=9224）
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const PORT = Number(process.env.WTX_PORT || 9224);
const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const SRC = path.join(REPO_ROOT, 'pkg/browser/find/element.go');

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
const send = (method, params = {}, sessionId) => new Promise((resolve, reject) => { const id = ++seq;
  pending.set(id, { resolve, reject }); ws.send(JSON.stringify({ id, method, params, sessionId })); });
const waitEvent = (method, sessionId, ms = 15000) => new Promise((resolve, reject) => { const t0 = Date.now();
  const tick = () => { const i = events.findIndex((e) => e.method === method && (!sessionId || e.sessionId === sessionId));
    if (i >= 0) return resolve(events.splice(i, 1)[0]); if (Date.now() - t0 > ms) return reject(new Error(`等待 ${method} 超时`)); setTimeout(tick, 50); }; tick(); });
await new Promise((r, j) => { ws.onopen = r; ws.onerror = j; });

const { targetId } = await send('Target.createTarget', { url: 'about:blank' });
const { sessionId } = await send('Target.attachToTarget', { targetId, flatten: true });
await send('Page.enable', {}, sessionId);
await send('Emulation.setDeviceMetricsOverride', { width: 1440, height: 1000, deviceScaleFactor: 1, mobile: false }, sessionId);
const evaluate = async (expression) => { const r = await send('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true }, sessionId);
  if (r.exceptionDetails) throw new Error('evaluate 异常: ' + JSON.stringify(r.exceptionDetails)); return r.result.value; };

// 每个用例：先铺 DOM，再对目标元素中心取证。
const CASES = [
  { id: 'A', label: '命中元素自身有文本（button）', expect: 'Sign in', html: `<button id="t" style="width:220px;height:60px;font:24px Helvetica">Sign in</button>`, pick: '#t' },
  { id: 'B', label: 'canvas ＋ 旁支段落（旧口径假 miss 案例）', expect: '', html: `<div id="panel" style="border:1px solid #ccc;padding:16px;width:420px"><p id="filler">This paragraph is unrelated filler copy inside the panel.</p><canvas id="t" width="260" height="90"></canvas></div>`, pick: '#t', canvas: 'Sign in' },
  { id: 'C', label: 'canvas 无任何祖先文本', expect: '', html: `<canvas id="t" width="260" height="90"></canvas>`, pick: '#t', canvas: 'Sign in' },
  { id: 'D', label: '图标链接 <a><img></a> 与兄弟链接同容器', expect: '', html: `<nav id="nav" style="display:flex;gap:24px"><a id="t" href="#"><img id="im" alt="Homepage" width="32" height="32" style="display:block" src="data:image/gif;base64,R0lGODlhAQABAIAAAP///wAAACH5BAEAAAAALAAAAAABAAEAAAICRAEAOw=="></a><a href="#">Domains</a><a href="#">Protocols</a></nav>`, pick: '#im' },
  { id: 'E', label: '链上祖先自有直接文本、无旁支（仍认证据）', expect: 'Home', html: `<a id="t" href="#" style="display:inline-block">Home<img id="im" alt="" width="24" height="24" style="display:inline-block;vertical-align:middle" src="data:image/gif;base64,R0lGODlhAQABAIAAAP///wAAACH5BAEAAAAALAAAAAABAAEAAAICRAEAOw=="></a>`, pick: '#im' },
  { id: 'F', label: '命中元素是带文本的后代（span）', expect: 'Newsletter', html: `<a id="t" href="#"><span id="sp">Newsletter</span></a>`, pick: '#sp' },
];

const LEGACY_JS = `(() => {
  const norm = function (s) { return s == null ? '' : String(s).replace(/\\s+/g, ' ').trim(); };
  const el = document.getElementById('t') || document.querySelector('img,span,canvas,button');
  let text = ''; try { let m = el; for (let up = 0; m && up <= 3; up++) { text = norm(m.innerText); if (text) break; m = m.parentElement; } } catch (e) { text = ''; }
  return text; })()`;

const out = { browser: ver.Browser, source: SRC, chars: ELEMENT_AT_POINT_JS.length, cases: [], failures: [] };
for (const c of CASES) {
  await evaluate(`document.body.innerHTML = ${JSON.stringify(c.html)};`);
  if (c.canvas) await evaluate(`(() => { const cv = document.getElementById('t'); const ctx = cv.getContext('2d');
    ctx.fillStyle = '#0b5fff'; ctx.fillRect(0,0,260,90); ctx.fillStyle = '#fff'; ctx.font = 'bold 36px Helvetica';
    ctx.textBaseline = 'middle'; ctx.fillText(${JSON.stringify(c.canvas)}, 30, 45); })()`);
  const box = JSON.parse(await evaluate(`(() => { const el = document.querySelector(${JSON.stringify(c.pick)});
    el.scrollIntoView({block:'center'}); const b = el.getBoundingClientRect();
    return JSON.stringify({ x: Math.round(b.left + b.width/2), y: Math.round(b.top + b.height/2) }); })()`));
  const raw = await evaluate(atPoint(box.x, box.y));
  const legacy = await evaluate(LEGACY_JS);
  const pass = raw.text === c.expect;
  out.cases.push({ id: c.id, label: c.label, point: box, expect: c.expect, hitPoint: raw,
    legacyText: legacy, legacyDiffers: legacy !== raw.text, pass });
  if (!pass) out.failures.push(`${c.id}: text=${JSON.stringify(raw.text)} want ${JSON.stringify(c.expect)}`);
}
out.verdict = out.failures.length === 0 ? 'ALL_PASS' : 'FAIL';
await send('Target.closeTarget', { targetId });
console.log(JSON.stringify(out, null, 2));
ws.close();
