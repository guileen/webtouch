#!/usr/bin/env node
// Read-only CDP network observer for WorkBuddy's own browser session.
// It intentionally never records request/response bodies or credential values.

const endpoint = new URL(process.env.WORKBUDDY_CDP_ENDPOINT || "http://127.0.0.1:9222");
const suffixes = (process.env.WORKBUDDY_HOST_SUFFIXES || "workbuddy.cn,codebuddy.cn,codebuddy.ai")
  .split(",").map(value => value.trim().toLowerCase()).filter(Boolean);
const durationMs = Number(process.env.WORKBUDDY_OBSERVER_MS || 5 * 60_000);
const seenTargets = new Set();
const activeSockets = new Set();
const requests = new Map();
const startedAt = Date.now();

function safePath(raw) {
  try {
    const url = new URL(raw);
    const path = url.pathname.split("/").map(segment => {
      if (/^[0-9]+$/.test(segment) || /^[0-9a-f]{8}-[0-9a-f-]{27,}$/i.test(segment) || segment.length >= 32) return ":id";
      return segment;
    }).join("/");
    return { host: url.hostname.toLowerCase(), path: path.slice(0, 240) };
  } catch {
    return { host: "<invalid-url>", path: "" };
  }
}

function isAllowed(host) {
  return suffixes.some(suffix => host === suffix || host.endsWith(`.${suffix}`));
}

function emit(record) {
  process.stdout.write(`${JSON.stringify({ at: new Date().toISOString(), ...record })}\n`);
}

async function attach(target) {
  if (!target.webSocketDebuggerUrl || seenTargets.has(target.id)) return;
  seenTargets.add(target.id);
  const socket = new WebSocket(target.webSocketDebuggerUrl);
  activeSockets.add(socket);
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
      return;
    }

    if (message.method === "Network.requestWillBeSent") {
      const { requestId, request, type, timestamp } = message.params;
      const safe = safePath(request.url);
      if (!isAllowed(safe.host)) return;
      const headerNames = Object.keys(request.headers || {}).map(name => name.toLowerCase()).sort();
      const authHeader = Object.entries(request.headers || {}).find(([name]) => name.toLowerCase() === "authorization")?.[1];
      const authScheme = typeof authHeader === "string" ? authHeader.trim().split(/\s+/, 1)[0].toLowerCase() : null;
      requests.set(requestId, { safe, method: request.method, type, timestamp, authScheme, cookieHeaderPresent: headerNames.includes("cookie") });
      emit({ event: "request", targetType: target.type, ...safe, method: request.method, resourceType: type, headerNames, authScheme, cookieHeaderPresent: headerNames.includes("cookie") });
      return;
    }

    if (message.method === "Network.responseReceived") {
      const { requestId, response, type, timestamp } = message.params;
      const request = requests.get(requestId);
      if (!request) return;
      request.responseAt = timestamp;
      request.status = response.status;
      request.responseType = type;
      return;
    }

    if (message.method === "Network.loadingFinished" || message.method === "Network.loadingFailed") {
      const { requestId, timestamp, errorText } = message.params;
      const request = requests.get(requestId);
      if (!request) return;
      requests.delete(requestId);
      emit({
        event: message.method === "Network.loadingFinished" ? "complete" : "failed",
        ...request.safe,
        method: request.method,
        resourceType: request.type,
        status: request.status ?? null,
        elapsedMs: Math.max(0, Math.round(((request.responseAt ?? timestamp) - request.timestamp) * 1000)),
        error: errorText ? String(errorText).slice(0, 100) : undefined,
      });
    }
  });

  const call = (method, params = {}) => new Promise((resolve, reject) => {
    const id = ++nextId;
    pending.set(id, { resolve, reject });
    socket.send(JSON.stringify({ id, method, params }));
  });
  await call("Network.enable");
  emit({ event: "attached", targetType: target.type, pageHost: safePath(target.url || "").host });
}

async function main() {
  emit({ event: "observer_started", endpoint: `${endpoint.origin}`, hostSuffixes: suffixes, durationMs });
  while (Date.now() - startedAt < durationMs) {
    const response = await fetch(new URL("/json/list", endpoint));
    if (!response.ok) throw new Error(`CDP target list returned HTTP ${response.status}`);
    const targets = await response.json();
    for (const target of targets) {
      if (target.type !== "page") continue;
      const pageHost = safePath(target.url || "").host;
      if (isAllowed(pageHost)) await attach(target);
    }
    await new Promise(resolve => setTimeout(resolve, 500));
  }
}

try {
  await main();
} catch (error) {
  emit({ event: "observer_error", message: String(error?.message || error).slice(0, 200) });
  process.exitCode = 1;
} finally {
  for (const socket of activeSockets) if (socket.readyState === WebSocket.OPEN) socket.close();
  emit({ event: "observer_stopped" });
}
