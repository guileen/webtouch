// WebTouch · 指纹探针本地靶站（研究用）
//
// 为什么需要它：浏览器指纹的一半在「连接层给出去的 HTTP 头」（UA / Sec-CH-UA / Accept-Language
// / Sec-Fetch-* / 头顺序），这一层页面 JS 看不到，只能由服务端记录。本服务既当被测页面，
// 又把每个请求的原始头（rawHeaders，保留顺序）存起来，供 cdp-probe.mjs 事后取走。
//
// 用法：node header-server.mjs [--port 8799] [--dump /tmp/headers.json]
import http from 'node:http';
import { writeFileSync } from 'node:fs';

const argv = process.argv.slice(2);
const opt = (n, d) => { const i = argv.indexOf(`--${n}`); return i >= 0 && argv[i + 1] ? argv[i + 1] : d; };
const port = Number(opt('port', '8799'));
const dumpPath = opt('dump', '');
const records = [];

const PAGE = `<!doctype html><html><head><meta charset="utf-8"><title>uws-fp-probe</title></head>
<body><h1>WebTouch fingerprint probe target</h1><canvas id="c" width="240" height="60"></canvas>
<iframe src="/frame" width="200" height="80"></iframe><img src="/pixel.png" alt="px">
<script>window.__probe_ok = true;</script></body></html>`;
const PNG_1PX = Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==', 'base64');

const server = http.createServer((req, res) => {
  if (req.url.startsWith('/__dump')) {
    const body = JSON.stringify(records, null, 2);
    if (dumpPath) writeFileSync(dumpPath, body);
    res.setHeader('content-type', 'application/json'); res.end(body); return;
  }
  records.push({
    at: new Date().toISOString(), method: req.method, url: req.url, httpVersion: req.httpVersion,
    headerOrder: (() => { const out = []; for (let i = 0; i < req.rawHeaders.length; i += 2) out.push(req.rawHeaders[i]); return out; })(),
    rawHeaders: req.rawHeaders, headers: req.headers, tls: req.socket.encrypted ? req.socket.getProtocol() : null,
  });
  if (req.url === '/pixel.png') { res.setHeader('content-type', 'image/png'); res.end(PNG_1PX); return; }
  if (req.url === '/frame') { res.setHeader('content-type', 'text/html'); res.end('<!doctype html><title>frame</title><p>frame</p>'); return; }
  res.setHeader('content-type', 'text/html; charset=utf-8'); res.end(PAGE);
});

server.listen(port, '127.0.0.1', () => console.error(`[header-server] listening http://127.0.0.1:${port}/ (dump: /__dump)`));
