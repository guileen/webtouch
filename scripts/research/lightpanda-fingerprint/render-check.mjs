// WebTouch · 渲染保真度小检（研究用）
//
// 指纹面之外的第二问：它画出来的东西，跟真 Chrome 差多少？对 WebTouch 的「截图 / 小窗直播 /
// VLM 看图」链路直接相关。做法：同一张含背景色块 / 绝对定位 / 中文 / canvas 的页面，
// 分别截图 + 读几何 + 读 canvas 像素。
//
// 用法：node render-check.mjs --endpoint http://127.0.0.1:9225 --label lightpanda --out-prefix out/render-lightpanda
import { writeFileSync } from 'node:fs';

const argv = process.argv.slice(2);
const opt = (n, d = '') => { const i = argv.indexOf(`--${n}`); return i >= 0 && argv[i + 1] ? argv[i + 1] : d; };
const endpoint = opt('endpoint').replace(/\/$/, '');
const label = opt('label', 'x');
const prefix = opt('out-prefix', label);

const PAGE = `<!doctype html><html><head><meta charset="utf-8"><title>render-check</title><style>
body{margin:0;font-family:Arial,Helvetica,sans-serif}
#abs{position:absolute;left:17px;top:33px;width:120px;height:40px;background:#e11}
#rel{position:relative;left:5px;top:7px;width:60px;height:30px;background:#1e1}
.big{font-size:40px;color:#123;font-weight:700}
</style></head><body>
<div id="abs">ABS-BOX</div><div id="rel">REL</div><div class="big">Hello 大字 40px</div>
<canvas id="cv" width="32" height="32"></canvas>
<script>
const cv=document.getElementById('cv'),ctx=cv.getContext('2d');
ctx.fillStyle='#f00';ctx.fillRect(0,0,16,16);
window.__cv={px:Array.from(ctx.getImageData(0,0,1,1).data),urlLen:cv.toDataURL().length,dataUrlHash:(()=>{const s=cv.toDataURL();let h=0;for(let i=0;i<s.length;i++){h=(h*31+s.charCodeAt(i))>>>0;}return h.toString(16);})()};
</script></body></html>`;

const ver = await (await fetch(`${endpoint}/json/version`)).json();
const ws = new WebSocket(ver.webSocketDebuggerUrl);
let seq = 0; const pending = new Map();
ws.addEventListener('message', (ev) => { const m = JSON.parse(ev.data); if (m.id && pending.has(m.id)) { pending.get(m.id)(m); pending.delete(m.id); } });
await new Promise((res, rej) => { ws.addEventListener('open', res, { once: true }); ws.addEventListener('error', () => rej(new Error('ws error')), { once: true }); });
const send = (method, params = {}, sessionId) => new Promise((res) => { const id = ++seq; pending.set(id, res); ws.send(JSON.stringify(sessionId ? { id, method, params, sessionId } : { id, method, params })); });

const target = await send('Target.createTarget', { url: 'about:blank' });
const att = await send('Target.attachToTarget', { targetId: target.result.targetId, flatten: true });
const sid = att.result.sessionId;
await send('Page.enable', {}, sid); await send('Runtime.enable', {}, sid);
await send('Page.navigate', { url: 'data:text/html;charset=utf-8,' + encodeURIComponent(PAGE) }, sid);
await new Promise((r) => setTimeout(r, 1800));

const expr = `(()=>{const g=id=>{const e=document.getElementById(id);const r=e.getBoundingClientRect();const cs=getComputedStyle(e);return {x:r.x,y:r.y,w:r.width,h:r.height,bg:cs.backgroundColor,display:cs.display};};
const big=document.querySelector('.big').getBoundingClientRect();
return {abs:g('abs'),rel:g('rel'),big:{x:big.x,y:big.y,w:big.width,h:big.height},hitAbs:(document.elementFromPoint(20,40)||{}).id||null,cv:window.__cv,bodyScrollHeight:document.documentElement.scrollHeight};})()`;
const r = await send('Runtime.evaluate', { expression: expr, returnByValue: true }, sid);
const out = { label, endpoint, cdp_version: ver, geometry: r.result?.result?.value ?? r };
const shot = await send('Page.captureScreenshot', { format: 'png' }, sid);
if (shot.result) { writeFileSync(`${prefix}.png`, Buffer.from(shot.result.data, 'base64')); out.screenshot = `${prefix}.png`; }
else out.screenshot = `failed: ${JSON.stringify(shot.error || shot)}`;
writeFileSync(`${prefix}.json`, JSON.stringify(out));
console.error(`[render-check:${label}] wrote ${prefix}.json` + (out.screenshot?.endsWith('.png') ? ` + ${out.screenshot}` : ''));
ws.close();
