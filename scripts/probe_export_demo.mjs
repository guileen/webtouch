#!/usr/bin/env node
// UWS 混合指挥演示 B：WASM 探针 → 极简 AXTree（TDD-WASM-09 / ExportToVLMFormat）。
// 先 `make probe-wasm`，再 `node scripts/probe_export_demo.mjs`。
// 产物缺失或旧版（未导出 uws_probe_export_vlm）时友好提示并正常退出。

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const wasmPath = path.resolve(__dirname, "../build/probe_wasm_wasm/probe_wasm.wasm");

if (!fs.existsSync(wasmPath)) {
  console.log("[probe-export-demo] 未找到 probe_wasm.wasm，先执行：make probe-wasm");
  process.exit(0);
}

const { instance } = await WebAssembly.instantiate(fs.readFileSync(wasmPath), {});
const e = instance.exports;
if (typeof e.uws_probe_export_vlm !== "function") {
  console.log("[probe-export-demo] 当前 WASM 未导出 uws_probe_export_vlm，请重新构建：make probe-wasm");
  process.exit(0);
}

const mem = new Uint8Array(e.memory.buffer);
const scratch = e.uws_probe_scratch();
const enc = new TextEncoder();

// 19 字段固定序（见 spec/algorithm-kernel.md §5 / probe.h kFieldCount）
const node = (tag, role, text, viewport, ast = "", cls = "", id = "") =>
  [tag, role, text, "", "", "", "", "", "", "0", "", "0", "0",
   viewport ? "1" : "0", "", ast, "", cls, id].join("\x1f") + "\x1e";

let payload = "";
payload += node("button", "button", "立即购买", true, 'function buy(){ fetch("/buy"); }', "btn", "buy-btn");
payload += node("input", "textbox", "验证码", true, "", "captcha", "captcha-input");
payload += node("a", "link", "隐私条款", false, "", "link", "tos");
payload += node("div", "", "", false, "", "decor", "ad-1"); // 装饰节点：导出时被过滤

const pb = enc.encode(payload);
mem.set(pb, scratch);
const bulkRet = e.uws_probe_nodes_bulk(0, pb.length);
if (bulkRet !== 0) {
  console.error(`[probe-export-demo] bulk 解析失败：${bulkRet}`);
  process.exit(1);
}
e.uws_probe_finalize();

const cap = e.uws_probe_scratch_size();
const n = e.uws_probe_export_vlm(0, cap);
if (n < 0) {
  console.error("[probe-export-demo] 导出失败");
  process.exit(1);
}
const tree = new TextDecoder().decode(mem.subarray(scratch, scratch + n));

console.log(
  `[probe-export-demo] kept=${e.uws_probe_kept_count()} ` +
  `truncated=${e.uws_probe_truncated()} export_bytes=${n}`
);
console.log("--- 极简 AXTree（VLM 指挥官输入）---");
console.log(tree);
