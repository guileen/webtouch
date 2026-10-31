// 页内快照/GT 判据真机夹具（§5.0.13 条款 7 对称骨架 ＋ §5.0.14 条款 1③ 枢轴解析、条款 2 三级判据）。
// 真机 = 本机自建 headless Chrome（每次运行独立临时 profile 与独立调试端口），**只读**：
// 不接管、不写入、不关闭任何既有会话；端口 9222 / 9224 被显式拒绝（宿主交互域零侵入，CON-217）。
//
// 单一事实源：页内脚本不在此复制，而是自 pkg/browser/cdp_session.go 的 `const pageSnapshotJS = \`...\`` 提取，
// 保证夹具测的就是引擎实际下发的脚本。
//
// 前置：本机装有 Chrome（默认 /Applications/Google Chrome.app/Contents/MacOS/Google Chrome，
//       可用 UWS_FIXTURE_CHROME 覆盖）；Node ≥ 22（依赖全局 fetch / WebSocket）。无 npm 依赖。
// 运行：node tests/fixtures/snapshot_harness/harness.mjs
// 期望：末行 `fixtures=13 pass=13 fail=0`，退出码 0。
import { spawn } from "node:child_process";
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), "../../..");
const CHROME = process.env.UWS_FIXTURE_CHROME ||
  "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome";
const PORT = Number(process.env.UWS_FIXTURE_PORT || 9333);
if (PORT === 9222 || PORT === 9224) {
  console.error(`拒绝端口 ${PORT}：属宿主/既有自建实例域（CON-217），夹具只允许自建独立端口`);
  process.exit(2);
}
const TMP = mkdtempSync(join(tmpdir(), "wtx-snapshot-harness-"));
const PROFILE = join(TMP, "chrome-profile");
const CASES_DIR = join(TMP, "cases");
mkdirSync(CASES_DIR, { recursive: true });

const raw = readFileSync(`${ROOT}/pkg/browser/cdp_session.go`, "utf8");
const marker = "const pageSnapshotJS = `";
const start = raw.indexOf(marker);
if (start < 0) { console.error("未能在 pkg/browser/cdp_session.go 找到 pageSnapshotJS（单一事实源缺失）"); process.exit(2); }
const jsTemplate = raw.slice(start + marker.length, raw.indexOf("`", start + marker.length));

const BUTTON = "<button data-testid='buy' style='position:absolute;left:100px;top:100px;width:200px;height:60px'>" +
  "<svg viewBox='0 0 10 10' width='12' height='12'><path d='M0 0h10v10H0z'/></svg><span>Buy now</span></button>";

const cases = [
  { name: "kb-selector-svg-span", body: BUTTON, sel: "[data-testid='buy']", tag: "", text: "",
    expect: { gt_index: 0, truncated: false } },
  { name: "fuzzy-tag-text", body: BUTTON, sel: "", tag: "button", text: "Buy now",
    expect: { gt_index: 0, truncated: false } },
  { name: "ambiguous-text-no-pivot", body: "<button>Buy</button><button>Buy</button>", sel: "", tag: "button", text: "Buy",
    expect: { gt_index: -1, truncated: true } },
  { name: "selector-miss-no-pivot", body: BUTTON, sel: "[data-testid='nope']", tag: "", text: "",
    expect: { gt_index: -1, truncated: true } },
  { name: "no-candidates", body: "<p>nothing interactive</p>", sel: "", tag: "", text: "",
    expect: { gt_index: -1, truncated: true, nodeCount: 0 } },
  { name: "geometry-fallback",
    body: "<div role='button' id='cand' style='position:absolute;left:0;top:0;width:200px;height:200px;z-index:1'>Target</div>" +
          "<div id='overlay' style='position:absolute;left:0;top:0;width:200px;height:200px;z-index:2'></div>",
    sel: "#cand", tag: "", text: "", expect: { gt_index: 0 } },
  { name: "hop-limit-vs-nearest",
    body: "<div role='button' id='near' style='position:absolute;left:380px;top:380px;width:120px;height:80px'>Near</div>" +
          "<div role='button' id='far' style='position:relative;left:0;top:0;width:100px;height:100px'>" +
          "<div><div><div><span style='position:absolute;left:400px;top:400px;width:60px;height:20px'>Tap</span></div></div></div></div>",
    sel: "", tag: "span", text: "Tap", expect: { gt_index: 0 } },
  // 判据甄别：两个候选的矩形都含命中点，只有 ② 祖先链能选中「Anc」（几何兜底只会给文档序靠前的「Other」）
  { name: "ancestor-tier-beats-geometry",
    body: "<div role='button' id='other' style='position:absolute;left:0;top:0;width:200px;height:120px'>Other</div>" +
          "<div role='button' id='anc' style='position:absolute;left:0;top:0;width:200px;height:120px'>Anc" +
          "<span style='display:block;width:200px;height:120px;background:rgba(0,0,0,0.1)'></span></div>",
    sel: "#anc", tag: "", text: "", expect: { gtIndexText: "Anc" } },
  // 边界：祖先恰好 3 阶（含）⇒ ② 命中；4 阶外不得命中（由上例的 hop-limit-vs-nearest 覆盖）
  { name: "ancestor-hop-3-boundary",
    body: "<div role='button' id='other' style='position:absolute;left:0;top:0;width:200px;height:120px'>Other</div>" +
          "<div role='button' id='anc3' style='position:absolute;left:0;top:0;width:200px;height:120px'>Deep" +
          "<div><div><span style='display:block;width:200px;height:120px;background:rgba(0,0,0,0.1)'></span></div></div></div>",
    sel: "#anc3", tag: "", text: "", expect: { gtIndexText: "Deep" } },
  // ── §5.0.14 决议 0：受体派发点为唯一锚点后的两个甄别例 ──
  // 受体落在 <svg>/<path>（非候选）上 ⇒ 只能靠 ② 祖先链命中按钮候选（index 1）：
  // 几何兜底只会给文档序靠前的 #other（index 0），故 gt_index=1 即证明祖先链生效。
  { name: "receptor-on-svg-ancestor-chain",
    body: "<div role='button' id='other' style='position:absolute;left:100px;top:100px;width:200px;height:60px'>Other</div>" +
          "<button id='buy' style='position:absolute;left:100px;top:100px;width:200px;height:60px;padding:0;border:0'>" +
          "<span style='display:block;width:200px;height:60px'>Buy now</span>" +
          "<svg viewBox='0 0 10 10' width='40' height='40' style='position:absolute;left:0;top:0'>" +
          "<path d='M0 0h10v10H0z'/></svg></button>",
    receptor: { x: 120, y: 120 }, hitTagIn: ["path", "svg"],
    sel: "", tag: "", text: "", expect: { gt_index: 1 } },
  // 受体点落在候选矩形之外的空白区 ⇒ 如实判 -1（全 false），**不得**用「矩形中心最近」编造 GT。
  { name: "receptor-on-blank-all-false",
    body: BUTTON, receptor: { x: 700, y: 500 },
    sel: "", tag: "", text: "", expect: { gt_index: -1 } },
  { name: "nearest-interactive-ancestor",
    body: "<div role='button' id='outer' style='position:absolute;left:0;top:0;width:300px;height:200px'>" +
          "<div role='button' id='inner' style='position:absolute;left:20px;top:20px;width:200px;height:100px'>" +
          "<span style='display:block;width:100px;height:40px'>Hit</span></div></div>",
    sel: "#inner", tag: "", text: "", expect: { gt_index: 0 } },
  // §5.0.13 条款 7：**对称挂载**——即使无受体/无枢轴，页面上**全部候选**也各挂自身 1 阶骨架
  // （消除「仅 GT 挂骨架 ⇒ 骨架成伪特征」，CON-214 硬线）。
  { name: "symmetric-neighbors-all-candidates",
    body: "<nav style='position:absolute;left:10px;top:10px;width:300px;height:120px'>" +
      "<a href='#1' style='display:block;height:30px'>One</a>" +
      "<a href='#2' style='display:block;height:30px'>Two</a>" +
      "<a href='#3' style='display:block;height:30px'>Three</a></nav>",
    sel: "", tag: "", text: "",
    expect: { nodeCount: 3, allNodesNeighborsNonEmpty: true, maxNeighbors: 3,
      firstNeighborRels: "parent,sibling,sibling" } },
];

function rpc(ws, id, method, params = {}, sessionId) {
  return new Promise((resolve, reject) => {
    const onMsg = (ev) => {
      const msg = JSON.parse(ev.data);
      if (msg.id !== id) return;
      ws.removeEventListener("message", onMsg);
      msg.error ? reject(new Error(`${method}: ${JSON.stringify(msg.error)}`)) : resolve(msg.result);
    };
    ws.addEventListener("message", onMsg);
    ws.send(JSON.stringify({ id, method, params, ...(sessionId ? { sessionId } : {}) }));
  });
}

const chrome = spawn(CHROME, ["--headless", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
  `--user-data-dir=${PROFILE}`, `--remote-debugging-port=${PORT}`, "about:blank"], { stdio: "ignore" });

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
function cleanup() {
  try { chrome.kill("SIGKILL"); } catch {}
  try { rmSync(TMP, { recursive: true, force: true }); } catch {}
}
process.on("exit", cleanup);

let version = null;
for (let i = 0; i < 40 && !version; i++) {
  try {
    const res = await fetch(`http://127.0.0.1:${PORT}/json/version`);
    if (res.ok) version = await res.json();
  } catch { await sleep(250); }
}
if (!version) { console.error("Chrome 未起来（调试端口不可达）"); process.exit(2); }

const ws = new WebSocket(version.webSocketDebuggerUrl);
await new Promise((res, rej) => { ws.addEventListener("open", res); ws.addEventListener("error", rej); });

let id = 0;
const results = [];
for (const c of cases) {
  const file = join(CASES_DIR, `case-${c.name}.html`);
  writeFileSync(file, "<!doctype html><html><head><meta charset='utf-8'>" +
    "<style>body{margin:0;font:14px sans-serif}</style></head><body>" + c.body + "</body></html>");
  const { targetId } = await rpc(ws, ++id, "Target.createTarget", { url: `file://${file}` });
  const { sessionId } = await rpc(ws, ++id, "Target.attachToTarget", { targetId, flatten: true });
  // 等新 target 的文档真正提交（否则会在初始空文档上下文里求值，候选恒为 0）。
  for (let i = 0; i < 40; i++) {
    const ready = await rpc(ws, ++id, "Runtime.evaluate",
      { expression: "location.href.startsWith('file://') && document.readyState === 'complete' && document.body && document.body.children.length > 0", returnByValue: true }, sessionId);
    if (ready.result.value === true) break;
    await sleep(100);
  }
  // 可选探针：确认受体点真正落在预期元素上（证明用例确实走在预期判据分支上）。
  if (c.hitTagIn) {
    const probe = await rpc(ws, ++id, "Runtime.evaluate",
      { expression: `(document.elementFromPoint(${c.receptor.x}, ${c.receptor.y}) || {}).tagName || ''`,
        returnByValue: true }, sessionId);
    const hitTag = String(probe.result.value || "").toLowerCase();
    if (!c.hitTagIn.includes(hitTag)) {
      results.push({ name: c.name, ok: false, why: `受体点命中元素 tag=${hitTag} 不在 ${JSON.stringify(c.hitTagIn)}` });
      await rpc(ws, ++id, "Target.closeTarget", { targetId });
      continue;
    }
  }
  const js = jsTemplate.replace("$RECEPTOR_HAS$", c.receptor ? "true" : "false")
    .replace("$RECEPTOR_X$", c.receptor ? String(c.receptor.x) : "0")
    .replace("$RECEPTOR_Y$", c.receptor ? String(c.receptor.y) : "0")
    .replace("$PIVOT_SEL$", JSON.stringify(c.sel))
    .replace("$PIVOT_TAG$", JSON.stringify(c.tag))
    .replace("$PIVOT_TEXT$", JSON.stringify(c.text));
  const r = await rpc(ws, ++id, "Runtime.evaluate", { expression: js, returnByValue: true, awaitPromise: true }, sessionId);
  await rpc(ws, ++id, "Target.closeTarget", { targetId });
  const got = r.result.value;
  if (!got || typeof got !== "object") {
    results.push({ name: c.name, ok: false, why: `无返回值: ${JSON.stringify(r.result)}` });
    continue;
  }
  const bad = [];
  for (const [k, want] of Object.entries(c.expect)) {
    const actual = k === "nodeCount" ? (got.nodes || []).length
      : k === "gtIndexText" ? ((got.nodes || [])[got.gt_index] || {}).text
      : k === "allNodesNeighborsNonEmpty" ? (got.nodes || []).length > 0 &&
        (got.nodes || []).every((n) => Array.isArray(n.neighbors) && n.neighbors.length > 0)
      : k === "maxNeighbors" ? Math.max(0, ...(got.nodes || []).map((n) => (n.neighbors || []).length))
      : k === "firstNeighborRels" ? (((got.nodes || [])[0] || {}).neighbors || []).map((n) => n.rel).join(",")
      : got[k];
    if (actual !== want) bad.push(`${k}=${JSON.stringify(actual)} 期望 ${JSON.stringify(want)}`);
  }
  results.push({ name: c.name, ok: bad.length === 0, why: bad.join("; "),
    info: `gt_index=${got.gt_index} truncated=${got.truncated} nodes=${(got.nodes || []).length}` +
      ` nb=${(got.nodes || []).map((n) => (n.neighbors || []).length).join("/")}` });
}
ws.close();
for (const r of results) console.log(`${r.ok ? "PASS" : "FAIL"} ${r.name}: ${r.ok ? r.info : r.why}`);
const bad = results.filter((r) => !r.ok);
console.log(`\nfixtures=${results.length} pass=${results.length - bad.length} fail=${bad.length}`);
process.exit(bad.length ? 1 : 0);
