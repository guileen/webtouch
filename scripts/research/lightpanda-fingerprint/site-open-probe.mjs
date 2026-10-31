// WebTouch · 真实站点「打得开吗」探针（研究用，只读）
//
// 与 cdp-probe.mjs 的分工：那个问"指纹面长什么样"，这个问"真实站点能不能打开、DOM 里到底有什么"。
// 只导航 + 读数据 + 截图，不点击、不登录、不提交表单。
//
// 用法：node site-open-probe.mjs --endpoint http://127.0.0.1:9223 --url https://example.com/ --label example --out out.json [--wait 9000] [--screenshot out.png]
import { writeFileSync } from 'node:fs';

const argv = process.argv.slice(2);
const opt = (n, d = '') => { const i = argv.indexOf(`--${n}`); return i >= 0 && argv[i + 1] ? argv[i + 1] : d; };
const endpoint = opt('endpoint', 'http://127.0.0.1:9223').replace(/\/$/, '');
const url = opt('url');
const label = opt('label', 'site');
const outPath = opt('out', '');
const wait = Number(opt('wait', '9000'));
const shotPath = opt('screenshot', '');
if (!url) { console.error('需要 --url'); process.exit(2); }

const ver = await (await fetch(`${endpoint}/json/version`)).json();
const ws = new WebSocket(ver.webSocketDebuggerUrl);
let seq = 0; const pending = new Map();
ws.addEventListener('message', (e) => { const m = JSON.parse(e.data); if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); } });
await new Promise((r, j) => { ws.addEventListener('open', r, { once: true }); ws.addEventListener('error', j, { once: true }); });
const send = (method, params = {}, sessionId) => new Promise((res) => { const id = ++seq; pending.set(id, res); ws.send(JSON.stringify(sessionId ? { id, method, params, sessionId } : { id, method, params })); });

const t0 = Date.now();
const created = await send('Target.createTarget', { url: 'about:blank' });
const attached = await send('Target.attachToTarget', { targetId: created.result.targetId, flatten: true });
const sid = attached.result.sessionId;
await send('Page.enable', {}, sid); await send('Runtime.enable', {}, sid);
await send('Page.navigate', { url }, sid);
await new Promise((r) => setTimeout(r, wait));

const EXPR = `(()=>{const q=s=>document.querySelectorAll(s).length;const bt=(document.body&&document.body.innerText||'').replace(/\\n{2,}/g,'\\n');
return {url:location.href,title:document.title,ready:document.readyState,nodes:document.querySelectorAll('*').length,
buttons:q('button'),links:q('a'),inputs:q('input'),textareas:q('textarea'),imgs:q('img'),
resources:performance.getEntriesByType('resource').length,
storageKeys:(()=>{try{return localStorage.length}catch(e){return 'ERR'}})(),
bodyText:bt.slice(0,1200)};})()`;
const r = await send('Runtime.evaluate', { expression: EXPR, returnByValue: true }, sid);
const report = { label, requested_url: url, elapsed_ms: Date.now() - t0, cdp_version: ver, page: r.result?.result?.value ?? r.result };
if (shotPath) {
  const shot = await send('Page.captureScreenshot', { format: 'png' }, sid);
  if (shot.result) { writeFileSync(shotPath, Buffer.from(shot.result.data, 'base64')); report.screenshot = shotPath; }
  else report.screenshot = `failed: ${JSON.stringify(shot.error)}`;
}
if (outPath) { writeFileSync(outPath, JSON.stringify(report)); console.error(`[site-open:${label}] wrote ${outPath}`); } else console.log(JSON.stringify(report, null, 1));
ws.close();
