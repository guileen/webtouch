// WebTouch · 指纹对照表生成器（研究用）
//
// 读入若干份 cdp-probe.mjs 产出的 JSON 快照，输出 Markdown 对照表 + 自动分歧清单。
// 用法：node compare.mjs --out out.md lightpanda=a.json chrome=b.json [uws=c.json]
import { readFileSync, writeFileSync } from 'node:fs';

const argv = process.argv.slice(2);
const outIdx = argv.indexOf('--out');
const outPath = outIdx >= 0 ? argv[outIdx + 1] : '';
const inputs = argv.filter((a, i) => !a.startsWith('--') && i !== outIdx + 1);
const reports = inputs.map((spec) => {
  const eq = spec.indexOf('=');
  return { label: spec.slice(0, eq), data: JSON.parse(readFileSync(spec.slice(eq + 1), 'utf8')) };
});

const val = (obj, path) => path.split('.').reduce((o, k) => (o == null ? undefined : o[k]), obj);
const fmt = (v) => {
  if (v === undefined) return '—';
  if (v === null) return 'null';
  if (typeof v === 'object') return JSON.stringify(v);
  return String(v);
};

const ROWS = [
  ['身份', 'UA', 'probe.navigator.userAgent'],
  ['身份', 'appVersion', 'probe.navigator.appVersion'],
  ['身份', 'vendor', 'probe.navigator.vendor'],
  ['身份', 'platform', 'probe.navigator.platform'],
  ['身份', 'product', 'probe.navigator.product'],
  ['身份', 'webdriver', 'probe.navigator.webdriver'],
  ['身份', 'hardwareConcurrency', 'probe.navigator.hardwareConcurrency'],
  ['身份', 'deviceMemory', 'probe.navigator.deviceMemory'],
  ['身份', 'maxTouchPoints', 'probe.navigator.maxTouchPoints'],
  ['身份', 'plugins.length', 'probe.navigator.plugins.length'],
  ['身份', 'mimeTypes.length', 'probe.navigator.mimeTypes.length'],
  ['UA-CH', 'brands', 'probe.navigator.userAgentData.brands'],
  ['UA-CH', 'mobile/platform', 'probe.navigator.userAgentData'],
  ['UA-CH', 'highEntropy(architecture/model)', 'probe.ua_ch_high_entropy.values'],
  ['语言', 'language', 'probe.navigator.language'],
  ['语言', 'languages', 'probe.navigator.languages'],
  ['语言', 'Intl.timeZone', 'probe.intl.timeZone'],
  ['语言', 'Intl.locale', 'probe.intl.locale'],
  ['屏幕', 'screen WxH / avail / depth', 'probe.screen'],
  ['屏幕', 'inner/outer/DPR', 'probe.viewport'],
  ['渲染', 'canvas2d hash', 'probe.canvas2d.hash'],
  ['渲染', 'canvas2d dataUrl头', 'probe.canvas2d.dataUrlHead'],
  ['渲染', 'canvas2d 可用', 'probe.canvas2d.available'],
  ['渲染', 'WebGL 可用', 'probe.webgl.available'],
  ['渲染', 'WebGL VENDOR:renderer', 'probe.webgl'],
  ['渲染', 'WebGL 扩展数', 'probe.webgl.extensionCount'],
  ['渲染', 'Audio 可用/hash', 'probe.audio'],
  ['渲染', '字体可探测数', 'probe.fonts.detectable'],
  ['布局', 'rect(100x50+pad+border)', 'probe.layout.rect'],
  ['布局', 'elementFromPoint 命中', 'probe.layout.elementFromPointHit'],
  ['布局', 'computed display/width', 'probe.layout.computedDisplay'],
  ['媒体', 'image decode', 'probe.image_decode'],
  ['媒体', 'mediaDevices', 'probe.media_devices'],
  ['媒体', 'permissions', 'probe.permissions.states'],
  ['存储', 'cookie 可写', 'probe.storage.cookieAfterWrite'],
  ['存储', 'indexedDB / caches / sessionStorage', 'probe.storage'],
  ['网络', 'WebRTC 可用/候选数', 'probe.webrtc'],
  ['反检测', 'navigator.userAgent getter 源码', 'probe.native_checks.toStringUserAgentGetter'],
  ['反检测', 'window.chrome', 'probe.native_checks.chromeObject'],
  ['反检测', 'cdc 类残留键', 'probe.native_checks.cdcKeys'],
  ['反检测', 'Error.stack 形态', 'probe.error_stack.head'],
  ['API 面', '已实现/总数/覆盖率', 'probe.api_surface'],
  ['DOM/CSS', 'querySelector/computed', 'probe.dom_css'],
  ['页面', 'title / readyState / body 长度', 'probe_doc'],
  ['页面', '截图 (Page.captureScreenshot)', 'steps.capture_screenshot'],
  ['连接层', '请求头顺序', 'http_headers'],
];

const value = (rep, path) => {
  if (path === 'probe_doc') {
    const m = rep.data.page_meta || {};
    return `${fmt(m.title)} / ${fmt(m.readyState)} / ${fmt(m.bodyHtmlLength)}`;
  }
  if (path === 'http_headers') {
    const hs = rep.data.http_headers;
    if (!Array.isArray(hs) || !hs.length) return '（无记录）';
    return hs[0].headerOrder.join(', ');
  }
  return fmt(val(rep.data, path));
};

const lines = [];
lines.push(`# 指纹面逐项对照（生成于 ${new Date().toISOString()}）`, '');
lines.push('## 端点信息', '');
for (const r of reports) lines.push(`- **${r.label}**：${JSON.stringify(r.data.cdp_version_endpoint)}`);
lines.push('', `## 逐项对照`, '');
lines.push(`| 维度 | 项目 | ${reports.map((r) => r.label).join(' | ')} |`);
lines.push('| --- | --- |' + reports.map(() => ' --- |').join(''));
for (const [dim, name, path] of ROWS) {
  lines.push(`| ${dim} | ${name} | ${reports.map((r) => value(r, path).replace(/\|/g, '\\|').slice(0, 88)).join(' | ')} |`);
}

lines.push('', '## 自动分歧清单', '');
const baseline = reports[reports.length - 1]; // 以「真 Chrome」为基准（放最后一个）
for (const r of reports.filter((x) => x !== baseline)) {
  const diff = ROWS.filter(([, , p]) => value(r, p) !== value(baseline, p));
  const ratio = val(r.data, 'probe.api_surface.ratio');
  lines.push(`- **${r.label}** vs **${baseline.label}**：${diff.length}/${ROWS.length} 项分歧；API 面覆盖率 ${fmt(ratio)}（${val(r.data, 'probe.api_surface.implemented')}/${val(r.data, 'probe.api_surface.total')}）`);
  lines.push(`  - 分歧项：${diff.map(([, n]) => n).join('、')}`);
}

const md = lines.join('\n') + '\n';
if (outPath) { writeFileSync(outPath, md); console.error(`[compare] wrote ${outPath}`); } else console.log(md);
