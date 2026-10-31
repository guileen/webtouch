// Fresh, isolated Chrome baseline for public-search-flow.js; no user profile or cookies.
import { spawn } from "node:child_process";

const chromePath = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome";
const port = Number(process.env.CHROME_PROBE_PORT || 19431);
const profile = process.env.CHROME_PROBE_PROFILE || `/private/tmp/lightpanda-research/chrome-public-search-${Date.now()}`;
const pageUrl = process.env.CHROME_PROBE_URL;
const chrome = spawn(chromePath, [
  "--headless=new", `--remote-debugging-port=${port}`, `--user-data-dir=${profile}`,
  "--no-first-run", "--no-default-browser-check", "--disable-extensions",
  "--disable-background-networking", "about:blank",
], { stdio: "ignore" });
let socket;

try {
  const started = performance.now();
  let version;
  for (let attempt = 0; attempt < 60; attempt++) {
    try {
      const response = await fetch(`http://127.0.0.1:${port}/json/version`);
      if (response.ok) { version = await response.json(); break; }
    } catch {}
    await new Promise(resolve => setTimeout(resolve, 250));
  }
  if (!version) throw new Error("Chrome DevTools endpoint did not start");

  const targetsResponse = await fetch(`http://127.0.0.1:${port}/json/list`);
  const targets = await targetsResponse.json();
  const target = targets.find(item => item.type === "page");
  if (!target) throw new Error("Chrome page target was not found");
  socket = new WebSocket(target.webSocketDebuggerUrl);
  await new Promise((resolve, reject) => {
    socket.addEventListener("open", resolve, { once: true });
    socket.addEventListener("error", reject, { once: true });
  });

  let nextId = 0;
  const pending = new Map();
  socket.addEventListener("message", event => {
    const message = JSON.parse(event.data);
    if (message.id && pending.has(message.id)) {
      const { resolve, reject } = pending.get(message.id);
      pending.delete(message.id);
      if (message.error) reject(new Error(message.error.message));
      else resolve(message.result);
    }
  });
  const call = (method, params = {}) => new Promise((resolve, reject) => {
    const id = ++nextId;
    pending.set(id, { resolve, reject });
    socket.send(JSON.stringify({ id, method, params }));
  });
  const evaluate = async expression => {
    const result = await call("Runtime.evaluate", { expression, awaitPromise: true, returnByValue: true });
    if (result.exceptionDetails) throw new Error(result.exceptionDetails.text || "page evaluation failed");
    return result.result.value;
  };

  await call("Page.enable");
  await call("Runtime.enable");
  await call("Page.navigate", { url: pageUrl || "https://quotes.toscrape.com/search.aspx" });
  for (let attempt = 0; attempt < 60; attempt++) {
    if (await evaluate("location.href !== 'about:blank' && document.readyState === 'complete'")) break;
    await new Promise(resolve => setTimeout(resolve, 250));
  }
  if (pageUrl) {
    if (new URL(pageUrl).hostname === 'quotes.toscrape.com') {
      for (let attempt = 0; attempt < 60; attempt++) {
        if (await evaluate("document.querySelectorAll('.quote').length > 0")) break;
        await new Promise(resolve => setTimeout(resolve, 250));
      }
    }
    const result = await evaluate(`({title:document.title,url:location.href,
      text:document.body.innerText.replace(/\\s+/g,' ').trim(),
      quoteTexts:Array.from(document.querySelectorAll('.quote .text')).map(q=>q.innerText.trim())})`);
    console.log(JSON.stringify({browser:version.Browser,resultUrl:result.url,
      elapsedMs:Math.round(performance.now()-started),result}));
  } else {
  await evaluate(`(() => { const a=document.querySelector('#author'); a.value='Albert Einstein'; a.dispatchEvent(new Event('change',{bubbles:true})); return true; })()`);
  let tagReady = false;
  for (let attempt = 0; attempt < 40; attempt++) {
    tagReady = await evaluate(`Array.from(document.querySelectorAll('#tag option')).some(o => /inspirational/i.test(o.text))`);
    if (tagReady) break;
    await new Promise(resolve => setTimeout(resolve, 250));
  }
  if (!tagReady) throw new Error("inspirational tag did not load");
  await evaluate(`(() => { const t=document.querySelector('#tag'); t.value=Array.from(t.options).find(o=>/inspirational/i.test(o.text)).value; t.dispatchEvent(new Event('change',{bubbles:true})); document.querySelector('input[type=submit]').click(); return true; })()`);
  let result;
  for (let attempt = 0; attempt < 60; attempt++) {
    result = await evaluate(`Array.from(document.querySelectorAll('.quote')).map(q => ({ text:q.innerText, author:q.querySelector('.author')?.innerText, tags:Array.from(q.querySelectorAll('.tag')).map(t=>t.innerText) }))`);
    if (result?.length) break;
    await new Promise(resolve => setTimeout(resolve, 250));
  }
  console.log(JSON.stringify({ browser: version.Browser, resultUrl: await evaluate("location.href"), elapsedMs: Math.round(performance.now() - started), result }));
  }
} finally {
  if (socket && socket.readyState === WebSocket.OPEN) socket.close();
  chrome.kill("SIGTERM");
}
