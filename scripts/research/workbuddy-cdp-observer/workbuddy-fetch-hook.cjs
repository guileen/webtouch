// Local research hook: observe metadata without changing task requests.
const fs = require("node:fs");
const path = require("node:path");
const cliEntry = /\/WorkBuddy\.app\/Contents\/Resources\/app\.asar\.unpacked\/cli\/(?:bin\/codebuddy|dist\/codebuddy(?:-headless)?\.js)$/;
const daemonEntry = /\/WorkBuddy\.app\/Contents\/Resources\/app\.asar\/main\/daemon-app-server-entry\.js$/;
const role = process.argv.some(arg => cliEntry.test(arg)) ? "cli"
  : process.argv.some(arg => daemonEntry.test(arg)) ? "daemon" : null;
const logDir = "/private/tmp/workbuddy-observer";
const logFile = path.join(logDir, "metadata.jsonl");

function record(metadata) {
  const line = JSON.stringify({ time: new Date().toISOString(), pid: process.pid, ...metadata }) + "\n";
  try { process.stderr.write("[WB_OBS]" + line); } catch {}
  try {
    fs.mkdirSync(logDir, { recursive: true, mode: 0o700 });
    fs.appendFileSync(logFile, line, { mode: 0o600, flag: "a" });
  } catch {}
}

function install() {
  if (!role) return { installed: false, reason: "unmatched_entry" };
  if (globalThis.__workbuddyObserver) return { installed: true, role, alreadyInstalled: true };
  const restorers = [];
  if (role === "daemon") {
    const cp = require("node:child_process");
    const originalSpawn = cp.spawn;
    cp.spawn = function observedSpawn(command, args, options) {
      if (Array.isArray(args) && args.some(arg => typeof arg === "string" && cliEntry.test(arg))) {
        const env = { ...(options?.env || process.env) };
        if (!String(env.NODE_OPTIONS || "").includes(__filename)) {
          env.NODE_OPTIONS = `${env.NODE_OPTIONS || ""} --require=${JSON.stringify(__filename)}`.trim();
        }
        const child = originalSpawn.call(this, command, args, { ...options, env });
        record({ event: "child_hook_forwarded", childPid: child.pid ?? null });
        return child;
      }
      return originalSpawn.apply(this, arguments);
    };
    require("node:module").syncBuiltinESMExports();
    const wrappedSpawn = cp.spawn;
    restorers.push(() => { if (cp.spawn === wrappedSpawn) cp.spawn = originalSpawn; });
  } else {
    const originalFetch = globalThis.fetch;
    if (typeof originalFetch !== "function") return { installed: false, reason: "fetch_unavailable" };
    globalThis.fetch = async function observedFetch(input, init) {
      let url, headers;
      let method = "GET";
      let bodyFields = [];
      try {
        url = new URL(typeof input === "string" || input instanceof URL ? input : input.url);
        method = String(init?.method || input?.method || "GET").toUpperCase();
        headers = new Headers(init?.headers || input?.headers || {});
        if (typeof init?.body === "string") {
          const body = JSON.parse(init.body);
          if (body && typeof body === "object" && !Array.isArray(body)) bodyFields = Object.keys(body).sort();
        }
      } catch {}
      const tool = url?.pathname.endsWith("/agenttool/v1/search") ? "WebSearch"
        : url?.pathname.endsWith("/agenttool/v1/webfetch") ? "WebFetch" : null;
      if (!tool) return originalFetch.apply(this, arguments);
      const started = Date.now();
      let status = null;
      let outcome = "error";
      try {
        const response = await originalFetch.apply(this, arguments);
        status = response.status;
        outcome = "response";
        return response;
      } finally {
        try {
          const auth = headers?.get("authorization") || "";
          const scheme = auth.match(/^([A-Za-z][A-Za-z0-9_-]{0,20})\s+\S/);
          record({ event: "tool_request", tool, host: url.hostname,
            path: url.pathname.replace(/\/(?:\d+|[a-f0-9-]{32,})(?=\/|$)/gi, "/:id"),
            method, headerNames: headers ? [...headers.keys()].sort() : [],
            authScheme: scheme ? scheme[1].toLowerCase() : auth ? "present" : null,
            cookieHeaderPresent: Boolean(headers?.has("cookie")), bodyFields,
            status, outcome, durationMs: Date.now() - started });
        } catch {}
      }
    };
    const wrappedFetch = globalThis.fetch;
    restorers.push(() => { if (globalThis.fetch === wrappedFetch) globalThis.fetch = originalFetch; });
  }
  globalThis.__workbuddyObserver = { role, uninstall() {
    restorers.forEach(restore => restore());
    require("node:module").syncBuiltinESMExports();
    delete globalThis.__workbuddyObserver;
    record({ event: "observer_removed", role });
  } };
  record({ event: "observer_ready", role });
  return { installed: true, role };
}

module.exports = { install, role, logFile };
install();
