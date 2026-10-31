// WebTouch · 通用 CDP 指纹探针（研究用，只读）
//
// 把 probe-payload.js 跑在任意 CDP 端点上，输出一份 JSON 快照。用于「Lightpanda 能否
// 作为 WebTouch 补充」的对照实验：同一份探针，分别打在 Lightpanda 与官方 Chrome 上。
//
// 用法：
//   node cdp-probe.mjs --endpoint http://127.0.0.1:9223 --url http://127.0.0.1:8799/ \
//        --label lightpanda --out out.json [--settle 2500] [--headers-dump URL] [--screenshot PNG]
//
// 依赖：Node 18+（用内置 fetch / WebSocket，无第三方依赖）。只做只读探测，不改页面逻辑。
import { readFileSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const argv = process.argv.slice(2);
const opt = (name, def = '') => {
  const i = argv.indexOf(`--${name}`);
  return i >= 0 && argv[i + 1] ? argv[i + 1] : def;
};
const endpoint = opt('endpoint', 'http://127.0.0.1:9223').replace(/\/$/, '');
const targetUrl = opt('url', 'http://127.0.0.1:8799/');
const label = opt('label', 'unknown');
const outPath = opt('out', '');
const settle = Number(opt('settle', '2500'));
const headersDump = opt('headers-dump', '');
const shotPath = opt('screenshot', '');
const payload = readFileSync(join(here, 'probe-payload.js'), 'utf8');

const log = (...a) => console.error(`[cdp-probe:${label}]`, ...a);
const withTimeout = (p, ms, what) => Promise.race([
  p, new Promise((_, rej) => setTimeout(() => rej(new Error(`timeout ${ms}ms: ${what}`)), ms)),
]);

async function connectCdp(wsUrl) {
  const ws = new WebSocket(wsUrl);
  let seq = 0;
  const pending = new Map();
  const listeners = [];
  ws.addEventListener('message', (ev) => {
    let msg; try { msg = JSON.parse(ev.data); } catch { return; }
    if (msg.id && pending.has(msg.id)) {
      const { resolve, reject } = pending.get(msg.id); pending.delete(msg.id);
      msg.error ? reject(new Error(`${msg.error.code}: ${msg.error.message}`)) : resolve(msg.result);
      return;
    }
    for (const l of listeners) l(msg);
  });
  await withTimeout(new Promise((res, rej) => {
    ws.addEventListener('open', res, { once: true });
    ws.addEventListener('error', () => rej(new Error('websocket error')), { once: true });
  }), 10000, 'ws open');
  const send = (method, params = {}, sessionId) => new Promise((resolve, reject) => {
    const id = ++seq;
    pending.set(id, { resolve, reject });
    ws.send(JSON.stringify(sessionId ? { id, method, params, sessionId } : { id, method, params }));
  });
  return { ws, send, on: (fn) => listeners.push(fn) };
}

async function main() {
  const report = { label, endpoint, target_url: targetUrl, probed_at: new Date().toISOString(), steps: {} };
  const verRes = await fetch(`${endpoint}/json/version`);
  report.cdp_version_endpoint = await verRes.json();
  const cdp = await connectCdp(report.cdp_version_endpoint.webSocketDebuggerUrl);

  let sessionId = null;
  try {
    const created = await cdp.send('Target.createTarget', { url: 'about:blank' });
    const attached = await cdp.send('Target.attachToTarget', { targetId: created.targetId, flatten: true });
    sessionId = attached.sessionId;
    report.steps.create_target = 'ok';
  } catch (e) {
    report.steps.create_target = `failed: ${e.message}`;
    // 退化路径：直接连 /json/list 里已有的 page target。
    const list = await (await fetch(`${endpoint}/json/list`)).json();
    const page = list.find((t) => t.type === 'page');
    if (!page) throw new Error('无可 attach 的 page target，且 Target.createTarget 失败');
    report.steps.fallback_ws = page.webSocketDebuggerUrl;
  }

  const navigated = new Promise((resolve) => {
    const timer = setTimeout(() => resolve('timeout'), 20000);
    cdp.on((msg) => {
      if (msg.method === 'Page.loadEventFired') { clearTimeout(timer); resolve('loadEventFired'); }
    });
  });
  for (const m of ['Page.enable', 'Runtime.enable']) {
    try { await cdp.send(m, {}, sessionId); report.steps[m] = 'ok'; } catch (e) { report.steps[m] = `failed: ${e.message}`; }
  }
  const nav = await cdp.send('Page.navigate', { url: targetUrl }, sessionId);
  report.steps.Page_navigate = nav && nav.errorText ? `errorText=${nav.errorText}` : 'ok';
  report.steps.load_event = await navigated;
  await new Promise((r) => setTimeout(r, settle));

  try {
    const res = await cdp.send('Runtime.evaluate', { expression: payload, awaitPromise: true, returnByValue: true }, sessionId);
    if (res.exceptionDetails) report.probe_error = res.exceptionDetails.exception?.description || JSON.stringify(res.exceptionDetails);
    report.probe = res.result ? res.result.value : null;
  } catch (e) {
    report.probe_error = `Runtime.evaluate failed: ${e.message}`;
  }

  try {
    const meta = await cdp.send('Runtime.evaluate', {
      expression: '({title: document.title, readyState: document.readyState, url: location.href, bodyHtmlLength: (document.body?document.body.innerHTML.length:-1), html: document.documentElement.outerHTML.slice(0,400)})',
      returnByValue: true,
    }, sessionId);
    report.page_meta = meta.result ? meta.result.value : null;
  } catch (e) { report.page_meta = { error: e.message }; }

  if (shotPath) {
    try {
      const shot = await cdp.send('Page.captureScreenshot', { format: 'png' }, sessionId);
      writeFileSync(shotPath, Buffer.from(shot.data, 'base64'));
      report.steps.capture_screenshot = 'ok';
    } catch (e) { report.steps.capture_screenshot = `failed: ${e.message}`; }
  }

  if (headersDump) {
    try { report.http_headers = await (await fetch(headersDump)).json(); }
    catch (e) { report.http_headers = { error: e.message }; }
  }

  cdp.ws.close();
  // 紧凑写出：这是机读原始读数，人类可读视图由 compare.mjs 生成；不缩进以免行数虚高。
  if (outPath) { writeFileSync(outPath, JSON.stringify(report)); log(`wrote ${outPath}`); }
  else console.log(JSON.stringify(report, null, 2));
}

main().catch((e) => { console.error(`[cdp-probe:${label}] FATAL ${e.stack || e}`); process.exit(1); });
