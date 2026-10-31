package recipe

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// defaultFReqTemplate 是 recipe 未声明模板时的历史兜底形态（3 元素内层数组）。
// 站点真实形态见 recipes/gemini.json（99 元素 sparse_array）；此处只保证老模板不失效。
const defaultFReqTemplate = `[null,{"json_string":[[["{{message}}",0,null,null,null,null,0],["en"],{"slot_array":"conv","prefix":"c_"}]]}]`

// signerSnippet 是 request.signer 的页内实现（契约 §13）：只调站点页面里的函数，
// 不重实现算法；失败一律 not_dispatched（fail-closed），保证外层可无损降级 DOM。
// 只在 recipe 声明了 signer 时注入；未声明的 recipe 脚本里不得出现签名调用。
const signerSnippet = `
    // 3.5 站点自带签名器（request.signer）：只调站点页面里的函数，不重实现算法。
    // 失败一律 not_dispatched（fail-closed），保证外层可无损降级 DOM。
    if (recipe.request && recipe.request.signer && recipe.request.signer.path) {
      try {
        const signer = recipe.request.signer;
        const signerPath = String(signer.path);
        const parts = signerPath.split(".");
        const fnName = parts.pop();
        const carrier = getPath(window, parts.join(".")) || window;
        const signFn = carrier[fnName];
        if (typeof signFn !== "function") throw new Error("signer_not_function: " + signerPath);
        let sigPromise = Promise.resolve(signFn.call(carrier, url));
        const signerTimeout = Number(signer.timeout_ms) || 0;
        if (signerTimeout > 0) {
          sigPromise = Promise.race([sigPromise, new Promise((_, reject) => {
            setTimeout(() => reject(new Error("signer_timeout")), signerTimeout);
          })]);
        }
        const sig = await sigPromise;
        if (!sig || typeof sig !== "object") throw new Error("signer_empty_result: " + signerPath);
        const signedUrl = sig[signer.url_from || "signedUrl"];
        if (typeof signedUrl === "string" && signedUrl !== "") url = signedUrl;
        const signedHeaders = sig[signer.headers_from || "signedHeader"];
        if (signedHeaders && typeof signedHeaders === "object") {
          for (const [hk, hv] of Object.entries(signedHeaders)) {
            if (hv !== undefined && hv !== null && hv !== "") headers[hk] = String(hv);
          }
        }
      } catch (e) {
        const msg = "signer_error: " + String(e);
        queue.error = msg;
        queue.done = true;
        return { status: "not_dispatched", error: msg };
      }
    }

`

// BuildFetchScript 根据 Recipe 与用户 Prompt，在页面上下文内生成完整的直发与流式监听 JS 闭包。
// 闭包执行时：
// 1. 在页面真实 window 内对所有 slots 动态求值（page_global、from_url、counter、cookie 等）；
// 2. 若关键槽位缺失且无 fallback，fail-closed 返回 status="not_dispatched"，触发外层无损降级；
// 3. 动态组装 URL、Headers、Body（支持 JSON 及 batchexecute urlencoded/f.req 复合体）；
// 4. 发起 window.fetch 并挂载 ReadableStream reader 至 window[streamQueueKey]；
// 5. 立即返回 { status: "streaming_started", ok: true }。
func BuildFetchScript(r *Recipe, prompt, model, streamQueueKey string) (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}

	recipeJSON, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("recipe: 无法序列化 recipe: %w", err)
	}

	// 未声明 signer 的 recipe 不注入签名段：脚本里连痕迹都不该有。
	signerBlock := ""
	if r.Request.Signer != nil {
		signerBlock = signerSnippet
	}

	// 注入的原生 JS 闭包
	script := fmt.Sprintf(`(async () => {
  const queueKey = %q;
  const promptInput = %s;
  const modelInput = %s;
  const recipe = %s;
  const defaultFReqTemplate = %s;

  const queue = window[queueKey] = { chunks: [], done: false, error: null };
  const controller = new AbortController();
  queue.controller = controller;

  let dispatched = false;
  try {
    // 1. 槽位求解辅助函数
    function getPath(obj, path) {
      if (!obj || !path) return undefined;
      const parts = path.split(".");
      let cur = obj;
      for (const p of parts) {
        if (cur == null) return undefined;
        cur = cur[p];
      }
      return cur;
    }

    // 模板求值：把 recipe 的声明式模板解成真实请求体结构。
    // 支持节点：字符串占位 {{slot}}（整串引用保持原值类型，数字槽位即数字）、
    // {"omit_if_empty":"name"}（空槽位整键不落 —— ChatGPT 首轮 body 没有 conversation_id 字段）、
    // {"json_string":X}（子结构序列化为字符串）、
    // {"if_slot":"name","then":X,"else":Y}（两态分支，如首轮无会话 ID）、
    // {"slot_array":"name","prefix":"c_"}（会话 ID 归一为 ["c_<16hex>"]）、
    // {"sparse_array":{"size":99,"set":{"3":"{{blob}}"}}}（稀疏长数组，其余位补 null）。
    function lookupSlot(name) {
      if (Object.prototype.hasOwnProperty.call(slotValues, name)) return slotValues[name];
      if (name === "prompt" || name === "message") return promptInput;
      if (name === "model") return modelInput;
      return undefined;
    }

    function resolveTemplateString(text) {
      const whole = /^\{\{([A-Za-z0-9_.-]+)\}\}$/.exec(text);
      if (whole) {
        const v = lookupSlot(whole[1]);
        return v === undefined ? null : v;
      }
      return text.replace(/\{\{([A-Za-z0-9_.-]+)\}\}/g, function (m, name) {
        const v = lookupSlot(name);
        return (v === undefined || v === null) ? "" : String(v);
      });
    }

    function resolveTemplateNode(node) {
      if (node === null || typeof node === "undefined") return null;
      if (typeof node === "string") return resolveTemplateString(node);
      if (typeof node !== "object") return node;
      if (Array.isArray(node)) {
        const arr = [];
        for (const item of node) arr.push(resolveTemplateNode(item));
        return arr;
      }
      if (Object.prototype.hasOwnProperty.call(node, "json_string")) {
        return JSON.stringify(resolveTemplateNode(node.json_string));
      }
      if (Object.prototype.hasOwnProperty.call(node, "if_slot")) {
        const branchVal = lookupSlot(node.if_slot);
        return resolveTemplateNode((branchVal === undefined || branchVal === null || branchVal === "") ? node["else"] : node.then);
      }
      if (Object.prototype.hasOwnProperty.call(node, "slot_array")) {
        const slotVal = lookupSlot(node.slot_array);
        if (slotVal === undefined || slotVal === null || slotVal === "") return null;
        const text = String(slotVal);
        const pfx = node.prefix || "";
        return [text.indexOf(pfx) === 0 ? text : pfx + text];
      }
      if (Object.prototype.hasOwnProperty.call(node, "sparse_array")) {
        const spec = node.sparse_array || {};
        const size = spec.size || 0;
        const set = spec.set || {};
        const arr = new Array(size).fill(null);
        for (const key of Object.keys(set)) {
          const idx = parseInt(key, 10);
          if (idx >= 0 && idx < size) arr[idx] = resolveTemplateNode(set[key]);
        }
        return arr;
      }
      if (Object.prototype.hasOwnProperty.call(node, "omit_if_empty")) {
        const name = String(node.omit_if_empty);
        const v = lookupSlot(name);
        if (v === undefined || v === null || v === "") return undefined;
        return resolveTemplateNode(node.value !== undefined ? node.value : ("{{" + name + "}}"));
      }
      const out = {};
      for (const key of Object.keys(node)) {
        const v = resolveTemplateNode(node[key]);
        if (v !== undefined) out[key] = v;
      }
      return out;
    }

    // 用已求解的槽位渲染一份 Headers（异步生产者取挑战 / 建会话时也要带同一套凭据与风控头）。
    // 未求解的占位（如 pow 槽位自己）一律跳过该头，避免发出空值头。
    function buildHeaders(override) {
      const out = {};
      for (const [hk, hv] of Object.entries(override || recipe.request.headers || {})) {
        let unresolved = false;
        const resolved = String(hv).replace(/\{\{([A-Za-z0-9_.-]+)\}\}/g, function (m, name) {
          const v = lookupSlot(name);
          if (v === undefined || v === null || v === "") { unresolved = true; return ""; }
          return String(v);
        });
        if (!unresolved) out[hk] = resolved;
      }
      return out;
    }

    function findFirstUUID(node) {
      const uuidRe = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
      const seen = new Set();
      const walk = (v) => {
        if (v === null || v === undefined) return undefined;
        if (typeof v === "string") return uuidRe.test(v) ? v : undefined;
        if (typeof v !== "object" || seen.has(v)) return undefined;
        seen.add(v);
        for (const item of (Array.isArray(v) ? v : Object.values(v))) {
          const hit = walk(item);
          if (hit) return hit;
        }
        return undefined;
      };
      return walk(node);
    }

    // PoW 解题：挑战取自站点接口，解题复用站点自己的 worker chunk（随站点版本自动跟新）。
    async function producePowChallenge(slot) {
      const targetPath = slot.target_path || slot.name;
      const resp = await window.fetch(slot.endpoint, {
        method: "POST",
        credentials: "include",
        headers: buildHeaders(),
        body: JSON.stringify({ target_path: targetPath })
      });
      const data = await resp.json();
      const challenge = getPath(data, slot.challenge_path || "data.biz_data.challenge");
      if (!challenge) throw new Error("pow_challenge_missing");
      const chunk = slot.chunk;
      const blobText = "importScripts=((i)=>(...a)=>i(...a.map((u)=>''+new URL(u,'" + chunk + "'))))(importScripts);importScripts('" + chunk + "')";
      const answer = await new Promise((resolve, reject) => {
        const w = new Worker(URL.createObjectURL(new Blob([blobText], { type: "text/javascript" })));
        const timer = setTimeout(() => { try { w.terminate(); } catch (e) {} reject(new Error("pow_timeout")); }, slot.timeout_ms || 30000);
        w.onmessage = (e) => {
          const d = e.data || {};
          if (d.type === "pow-answer") { clearTimeout(timer); try { w.terminate(); } catch (e) {} resolve(d.answer); }
          else if (d.type === "pow-error") { clearTimeout(timer); try { w.terminate(); } catch (e) {} reject(new Error("pow_error: " + String(d.error))); }
        };
        w.onerror = (e) => { clearTimeout(timer); try { w.terminate(); } catch (e) {} reject(new Error("pow_worker_error")); };
        // 字段名必须映射成 expireAt：站点 worker 按 expireAt 拼解题前缀，传原样 expire_at 会 "No solution found"。
        w.postMessage({ type: "pow-challenge", challenge: Object.assign({}, challenge, { expireAt: challenge.expire_at }) });
      });
      const payload = Object.assign({}, answer, { target_path: targetPath });
      return btoa(unescape(encodeURIComponent(JSON.stringify(payload))));
    }

    // 首轮建会话：页面 URL 已有会话 ID 就直接取，没有才调站点建会话接口。
    async function produceSessionMint(slot) {
      if (slot.re) {
        try {
          const m = new RegExp(slot.re).exec(location.pathname);
          if (m && m[1]) return m[1];
        } catch (e) {}
      }
      const resp = await window.fetch(slot.endpoint, {
        method: "POST",
        credentials: "include",
        headers: buildHeaders(),
        body: JSON.stringify({})
      });
      const data = await resp.json().catch(() => null);
      let minted = slot.id_path ? getPath(data, slot.id_path) : undefined;
      if (typeof minted !== "string" || minted === "") minted = findFirstUUID(data);
      if (typeof minted !== "string" || minted === "") throw new Error("session_mint_failed: HTTP " + resp.status);
      return minted;
    }

    // 槽位模板渲染（fetch_field 的 URL/请求体里可以引用已求解的槽位，如 {{session_id}}）。
    function renderSlots(text, encode) {
      let out = String(text == null ? "" : text);
      for (const [name, val] of Object.entries(slotValues)) {
        if (val === undefined || val === null) continue;
        const ph = "{{" + name + "}}";
        const repr = encode ? encodeURIComponent(String(val)) : String(val);
        while (out.indexOf(ph) >= 0) out = out.replace(ph, repr);
      }
      return out;
    }

    // 轮询会话对象补正文（response.kind=sse_handoff_poll 专用）：
    // 会话 ID 从前奏流原始文本里按 recipe 声明抽；正文取「最后一条助手消息」，
    // 且必须**不等于本轮请求的 parent_message_id**，避免把上一轮答案当本轮结果。
    async function runAnswerPoll(poll, raw) {
      let cid = "";
      if (poll.conversation_id_re) {
        const m = new RegExp(poll.conversation_id_re).exec(raw || "");
        if (m) cid = m[1] || m[0];
      }
      if (!cid) throw new Error("poll_conversation_id_missing");
      const pollUrl = String(poll.url_template).split("{{conversation_id}}").join(encodeURIComponent(cid));
      const interval = poll.interval_ms || 1500;
      const deadline = Date.now() + (poll.timeout_ms || 60000);
      const prior = slotValues["parent_message_id"];
      while (Date.now() < deadline) {
        try {
          const r = await window.fetch(pollUrl, { method: "GET", credentials: "include", headers: buildHeaders() });
          if (r.ok) {
            const data = await r.json().catch(() => null);
            const id = lastAssistantMessage(data, "id");
            const text = lastAssistantMessage(data, "text");
            if (text && id && id !== prior) {
              queue.chunks.push(JSON.stringify({ __uws_poll_answer: text }));
              return;
            }
          }
        } catch (e) { /* 轮询失败继续重试，直到 deadline */ }
        await new Promise((r) => setTimeout(r, interval));
      }
      throw new Error("poll_timeout");
    }

    // 派发前的只读取数（fetch_field）：先发一次页内请求，按 path 取字段。
    // 用于「值只在服务端」的场景（千问 topic_id：session/get 回带，页面无落点）。
    // optional 槽位取不到即返回 undefined（交由兜底/两态分支处理），不算槽位错误。
    async function produceFetchField(slot) {
      try {
        const url = renderSlots(slot.endpoint, true);
        const method = (slot.method || "POST").toUpperCase();
        const init = { method: method, credentials: "include", headers: buildHeaders(slot.headers) };
        if (method !== "GET" && method !== "HEAD" && slot.body) {
          init.body = renderSlots(slot.body, false);
          const ctype = slot.content_type || "application/json";
          const hasCT = Object.keys(init.headers).some((k) => k.toLowerCase() === "content-type");
          if (!hasCT) init.headers["Content-Type"] = ctype;
        }
        const resp = await window.fetch(url, init);
        const data = await resp.json().catch(() => null);
        let v = undefined;
        if (slot.select === "last_assistant_message_id") v = lastAssistantMessage(data, "id");
        else if (slot.select === "last_assistant_text") v = lastAssistantMessage(data, "text");
        else if (slot.path) v = getPath(data, slot.path);
        if (v === undefined || v === null || v === "") {
          if (slot.optional) return undefined;
          throw new Error("fetch_field_missing: HTTP " + resp.status);
        }
        return v;
      } catch (e) {
        // 可缺省槽位（如首轮还没有会话对象）不因网络/形状异常 fail-closed。
        if (slot.optional) return undefined;
        throw e;
      }
    }

    // 会话对象里最后一条助手消息（按 create_time 取最新）。事实源＝站点自述的 mapping，
    // 不是页面 DOM 快照；续轮 parent_message_id 与答案正文都取这里。
    function lastAssistantMessage(conv, want) {
      if (!conv || !conv.mapping) return undefined;
      let best = null;
      for (const node of Object.values(conv.mapping)) {
        const m = node && node.message;
        if (!m || !m.author || m.author.role !== "assistant") continue;
        if (!best || Number(m.create_time || 0) >= Number(best.message.create_time || 0)) best = node;
      }
      if (!best) return undefined;
      const msg = best.message;
      if (want === "id") return msg.id;
      const parts = (msg.content && msg.content.parts) || [];
      const text = parts.filter((x) => typeof x === "string").join("");
      return text === "" ? undefined : text;
    }

    const slotValues = {};
    for (const slot of recipe.slots || []) {
      let val = undefined;
      try {
      switch (slot.producer) {
        case "prompt":
          val = promptInput;
          break;
        case "model":
          val = modelInput || slot.fallback || "";
          break;
        case "static":
          val = slot.value !== undefined ? slot.value : slot.fallback;
          break;
        case "now_epoch":
          // 数字槽位：整串引用时保持 number（create_time 这类站点字段不接受字符串）。
          val = Date.now() / 1000;
          break;
        case "page_global":
          val = getPath(window, slot.path);
          break;
        case "from_url":
          try {
            const pathAndSearch = location.pathname + location.search;
            const re = new RegExp(slot.re);
            const m = re.exec(pathAndSearch);
            if (m && m[1]) {
              val = m[1];
            }
          } catch (e) {}
          break;
        case "counter":
          try {
            let maxReqId = 0;
            const res = performance.getEntriesByType("resource") || [];
            for (const r of res) {
              const m = /_reqid=(\d+)/.exec(r.name || "");
              if (m) maxReqId = Math.max(maxReqId, parseInt(m[1], 10));
            }
            if (window.__uws_last_reqid) {
              maxReqId = Math.max(maxReqId, window.__uws_last_reqid);
            }
            const step = slot.step || 100000;
            val = maxReqId ? maxReqId + step : 3000000;
            window.__uws_last_reqid = val;
          } catch (e) {
            val = 3000000;
          }
          break;
        case "fresh_uuid":
          try {
            val = crypto.randomUUID ? crypto.randomUUID() : (Date.now().toString(36) + Math.random().toString(36).slice(2));
          } catch (e) {
            val = Date.now().toString(36) + Math.random().toString(36).slice(2);
          }
          break;
        case "fresh_hex":
          try {
            const hexLen = slot.length || 32;
            const hexBuf = new Uint8Array(Math.ceil(hexLen / 2));
            if (crypto.getRandomValues) { crypto.getRandomValues(hexBuf); } else { for (let i = 0; i < hexBuf.length; i++) hexBuf[i] = Math.floor(Math.random() * 256); }
            let hexText = "";
            for (const byte of hexBuf) hexText += byte.toString(16).padStart(2, "0");
            val = hexText.slice(0, hexLen);
          } catch (e) {
            val = Date.now().toString(16) + Math.random().toString(16).slice(2);
          }
          break;
        case "fresh_blob":
          try {
            const blobLen = slot.bytes || 1024;
            const blobBuf = new Uint8Array(blobLen);
            if (crypto.getRandomValues) { crypto.getRandomValues(blobBuf); } else { for (let i = 0; i < blobLen; i++) blobBuf[i] = Math.floor(Math.random() * 256); }
            let bin = "";
            for (let i = 0; i < blobLen; i++) bin += String.fromCharCode(blobBuf[i]);
            val = btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
          } catch (e) {
            val = Date.now().toString(36) + Math.random().toString(36).slice(2);
          }
          break;
        case "cookie":
          try {
            const name = slot.key || slot.name;
            const m = new RegExp("(?:^|; )" + name + "=([^;]*)").exec(document.cookie);
            if (m) val = decodeURIComponent(m[1]);
          } catch (e) {}
          break;
        case "local_storage":
          try {
            const raw = localStorage.getItem(slot.key || slot.name);
            if (raw !== null && raw !== undefined) {
              let lv = raw;
              // decode=json：两层取值（JSON 字符串 -> 对象 -> path），解析失败退回原始串。
              if (slot.decode === "json") {
                try { lv = JSON.parse(raw); } catch (e) { lv = raw; }
              }
              if (slot.path) lv = getPath(lv, slot.path);
              if (lv !== undefined && lv !== null && typeof lv !== "object") val = lv;
            }
          } catch (e) {}
          break;
        case "pow_challenge":
          val = await producePowChallenge(slot);
          break;
        case "session_mint":
          val = await produceSessionMint(slot);
          break;
        case "fetch_field":
          val = await produceFetchField(slot);
          break;
        default:
          val = slot.fallback;
      }
      } catch (e) {
        // 异步生产者失败（取挑战/解题/建会话）发生在主请求派发之前：属 not_dispatched，可无损降级 DOM。
        const msg = "slot_producer_error: " + slot.name + ": " + String(e);
        queue.error = msg;
        queue.done = true;
        return { status: "not_dispatched", error: msg };
      }

      if (val === undefined || val === null || val === "") {
        if (slot.fallback !== undefined && slot.fallback !== null) {
          val = slot.fallback;
        } else if (slot.optional) {
          // 首轮可缺失的槽位（如 Gemini 会话 ID）：按 null 传，交给 body 构造器决定形态
          val = null;
        } else {
          // 必需槽位缺失 -> fail-closed 退出，不发起未授权或损坏的请求
          queue.error = "slot_missing: " + slot.name;
          queue.done = true;
          return { status: "not_dispatched", error: "slot_missing: " + slot.name };
        }
      }
      // 取值前缀：如会话 ID 归一为 c_<16hex>、不透明串的 "!"
      if (slot.prefix && typeof val === "string" && val !== "" && val.indexOf(slot.prefix) !== 0) {
        val = slot.prefix + val;
      }
      slotValues[slot.name] = val;
    }

    // 2. 构造目标 URL
    let url = recipe.request.url_template;
    for (const [name, val] of Object.entries(slotValues)) {
      const ph = "{{" + name + "}}";
      while (url.indexOf(ph) >= 0) {
        url = url.replace(ph, encodeURIComponent(String(val)));
      }
    }
    if (url.startsWith("/")) {
      url = location.origin + url;
    }

    // 3. 构造请求 Headers
    const headers = buildHeaders();

    // 3.5 站点自带签名器（request.signer）：只在 recipe 声明了 signer 时才注入（见 signerSnippet）。
%s    // 4. 构造 Body
    let body = null;
    const method = (recipe.request.method || "POST").toUpperCase();
    if (method !== "GET" && method !== "HEAD") {
      // 检查复合构造器：json_object（JSON 对象体，转义交给 JSON.stringify）/ json_outer（Google batchexecute 的 f.req）
      if (recipe.request.body_builders) {
        for (const [builderKey, builder] of Object.entries(recipe.request.body_builders)) {
          if (builder.kind === "json_object") {
            body = JSON.stringify(resolveTemplateNode(builder.template));
            headers["Content-Type"] = headers["Content-Type"] || "application/json";
          } else if (builderKey === "f.req" && builder.kind === "json_outer") {
          const tpl = (builder.template === undefined || builder.template === null) ? defaultFReqTemplate : builder.template;
          slotValues["f.req"] = JSON.stringify(resolveTemplateNode(tpl));
          }
        }
      }

      if (recipe.request.body_template) {
        let tpl = recipe.request.body_template;
        const isUrlEncoded = (headers["Content-Type"] || "").includes("application/x-www-form-urlencoded");
        for (const [name, val] of Object.entries(slotValues)) {
          const ph = "{{" + name + "}}";
          while (tpl.indexOf(ph) >= 0) {
            const repr = isUrlEncoded ? encodeURIComponent(String(val)) : (typeof val === "object" ? JSON.stringify(val) : String(val));
            tpl = tpl.replace(ph, repr);
          }
        }
        body = tpl;
      } else if (body === null) {
        // 默认按 JSON 发送
        body = JSON.stringify({
          message: promptInput,
          model: modelInput,
          stream: true
        });
        headers["Content-Type"] = headers["Content-Type"] || "application/json";
      }
    }

    // 5. 派发请求并流式监听
    dispatched = true;
    const resp = await window.fetch(url, {
      method: method,
      credentials: "include",
      signal: controller.signal,
      headers: headers,
      body: body
    });

    if (!resp.ok) {
      const errText = await resp.text().catch(() => "");
      queue.error = "HTTP " + resp.status + ": " + errText.slice(0, 500);
      queue.done = true;
      return { status: "error", httpStatus: resp.status, error: queue.error };
    }

    const reader = resp.body.getReader();
    queue.reader = reader;
    const decoder = new TextDecoder("utf-8");

    (async () => {
      try {
        let raw = "";
        while (true) {
          const { value, done } = await reader.read();
          if (done) break;
          const text = decoder.decode(value, { stream: true });
          raw += text;
          queue.chunks.push(text);
        }
        // 站点把答案正文搬到别的通道时（ChatGPT：stream_handoff 之后走 WebSocket conduit），
        // 按 recipe.response.poll 的声明把正文从站点会话对象读回来（前奏流本身不含正文）。
        const poll = recipe.response && recipe.response.poll;
        if (poll && poll.url_template) {
          try {
            await runAnswerPoll(poll, raw);
          } catch (e) {
            queue.chunks.push(JSON.stringify({ __uws_poll_error: String(e) }));
          }
        }
        queue.done = true;
      } catch (err) {
        // 读数诊断：能走到这里说明「响应头已到、读流阶段被中断」（站点/CF 侧断开），
        // 必须带上阶段与 httpStatus，否则错误体只剩 "TypeError: network error"，
        // 无法区分「提交动作失败」与「响应链路中断」，定位只能靠外部抓包。
        queue.error = "stream_interrupted_after_headers: " + String(err) + " (httpStatus=" + resp.status + ")";
        queue.done = true;
      }
    })();

    // 回显打了 echo 标记的槽位（只给非凭据槽位，如会话 ID）：两态验证与清账都要用它。
    const echoed = {};
    for (const slot of recipe.slots || []) {
      if (slot.echo && slotValues[slot.name] !== undefined && slotValues[slot.name] !== null) {
        echoed[slot.name] = String(slotValues[slot.name]);
      }
    }
    return { status: "streaming_started", ok: true, httpStatus: resp.status, slots: echoed };
  } catch (err) {
    // 读数诊断：带上「已派发 / 未派发」阶段——未派发可无损降级 DOM，已派发则不可重放（fail-closed）。
    const stage = dispatched ? "after_dispatch" : "before_dispatch";
    const msg = "fetch_failed[" + stage + "]: " + String(err);
    queue.error = msg;
    queue.done = true;
    return { status: dispatched ? "network_error" : "not_dispatched", error: msg };
  }
})()`,
		streamQueueKey,
		strconv.Quote(prompt),
		strconv.Quote(model),
		string(recipeJSON),
		defaultFReqTemplate,
		signerBlock,
	)

	return script, nil
}
