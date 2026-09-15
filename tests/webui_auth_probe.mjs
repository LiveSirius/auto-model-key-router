// WebUI 鉴权流程回归探针：用最小 DOM 垫片驱动真实的 webui ES 模块。
// 用法：node webui_auth_probe.mjs <scenario>，结果以 JSON 打到 stdout。
// 每个场景单独起进程，保证模块级缓存（各页面的 state）互不干扰。

import { pathToFileURL } from "node:url";
import path from "node:path";

const WEBUI = path.resolve(import.meta.dirname, "../auto_model_key_router/webui");
const scenario = process.argv[2];

// —— 最小 DOM 垫片 ——
class FakeNode {
  constructor(tag) {
    this.tagName = tag;
    this.children = [];
    this.attrs = {};
    this.listeners = {};
    this.className = "";
    this.style = {};
    this.value = "";
    this.parent = null;
  }
  append(...nodes) {
    for (const node of nodes.flat()) {
      if (node === null || node === undefined) continue;
      node.parent = this;
      this.children.push(node);
    }
  }
  appendChild(node) { this.append(node); return node; }
  replaceChildren(...nodes) { this.children = []; this.append(...nodes); }
  setAttribute(key, value) { this.attrs[key] = String(value); }
  getAttribute(key) { return this.attrs[key]; }
  addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); }
  removeEventListener(type, fn) {
    this.listeners[type] = (this.listeners[type] || []).filter((item) => item !== fn);
  }
  remove() {
    if (this.parent) this.parent.children = this.parent.children.filter((c) => c !== this);
  }
  querySelector() { return null; }
  focus() {}
  get textContent() {
    return this.children.map((c) => (c.textContent === undefined ? String(c) : c.textContent)).join(" ");
  }
}
class FakeText extends FakeNode {
  constructor(text) { super("#text"); this.data = text; }
  get textContent() { return this.data; }
}

const root = new FakeNode("div");
root.attrs.id = "root";

// Node 24 已内置只读的 navigator/location 全局，只能用 defineProperty 覆盖。
const define = (name, value) =>
  Object.defineProperty(global, name, { value, writable: true, configurable: true });

let reloads = 0;
define("location", { hash: "#/settings", reload() { reloads += 1; } });
define("window", { addEventListener() {}, isSecureContext: true, location: global.location });
define("navigator", { clipboard: null });

global.Node = FakeNode;
define("document", {
  createElement: (tag) => new FakeNode(tag),
  createElementNS: (_ns, tag) => new FakeNode(tag),
  createTextNode: (text) => new FakeText(text),
  getElementById: (id) => (id === "root" ? root : null),
  body: new FakeNode("body"),
  addEventListener() {},
  removeEventListener() {},
  execCommand: () => true,
});

// app.js 的 scheduleTimers 会拉起真实定时器，会把探针进程挂住；这里一律 stub。
define("setInterval", () => 0);
define("clearInterval", () => {});
define("setTimeout", () => 0);

const storage = new Map();
define("localStorage", {
  getItem: (k) => (storage.has(k) ? storage.get(k) : null),
  setItem: (k, v) => storage.set(k, String(v)),
  removeItem: (k) => storage.delete(k),
});

// —— 假服务端 ——
const server = {
  authEnabled: true,
  accepted: new Set(["good-key"]),
  requests: [],
};

function respond(status, payload) {
  return {
    ok: status >= 200 && status < 300,
    status,
    async text() { return payload === undefined ? "" : JSON.stringify(payload); },
  };
}

global.fetch = async (url, options = {}) => {
  const headers = options.headers || {};
  const bearer = (headers.Authorization || "").replace(/^Bearer /, "");
  server.requests.push({ url, bearer });

  if (url === "/health") {
    return respond(200, {
      status: "ok",
      version: "4.0.3",
      models: [],
      local_auth_enabled: server.authEnabled,
    });
  }
  if (server.authEnabled && !server.accepted.has(bearer)) {
    return respond(401, { detail: "本地 API key 验证失败" });
  }
  if (url.startsWith("/api/settings")) {
    return respond(200, {
      config_revision: "rev-1",
      settings: { host: "127.0.0.1", port: 28881, max_retries: 2, local_auth_enabled: true },
    });
  }
  return respond(200, {});
};

// —— 驱动 ——
const { boot, store, renderShell } = await import(pathToFileURL(path.join(WEBUI, "app.js")).href);
const { api } = await import(pathToFileURL(path.join(WEBUI, "api.js")).href);

function findAll(node, predicate, out = []) {
  if (predicate(node)) out.push(node);
  for (const child of node.children) findAll(child, predicate, out);
  return out;
}

const text = () => root.textContent;
const inputs = () => findAll(root, (n) => n.tagName === "input");
const buttons = () => findAll(root, (n) => n.tagName === "button");
const clickButton = async (label) => {
  const target = buttons().find((b) => b.textContent.trim() === label);
  if (!target) throw new Error(`找不到按钮: ${label}`);
  for (const handler of target.listeners.click || []) await handler({});
};
const submitKey = async (value) => {
  const field = inputs()[0];
  if (!field) throw new Error("登录卡没有 Key 输入框");
  field.value = value;
  await clickButton("连接");
};

// 预期：每个场景前置的 localStorage 与假服务端状态
const setup = {
  stale_key_prompts_login: () => { storage.set("amkr.apiKey", "stale-key"); },
  no_key_prompts_login: () => {},
  valid_key_renders_page: () => { storage.set("amkr.apiKey", "good-key"); },
  wrong_key_submit_shows_error: () => {},
  correct_key_submit_reloads: () => {},
  mid_session_401_returns_to_login: () => { storage.set("amkr.apiKey", "good-key"); },
  login_input_survives_health_poll: () => {},
  auth_disabled_no_login: () => {
    server.authEnabled = false;
    storage.set("amkr.apiKey", "anything");
  },
};
setup[scenario]?.();

await boot();

const checks = {};
if (scenario === "stale_key_prompts_login") {
  // 报告的问题：带着失效 Key 进入时，不应把 401 当成设置读取失败缓存下来。
  checks.loginCard = text().includes("连接到 AMKR");
  checks.notCachedSettingsError = !text().includes("读取设置失败");
  checks.unauthorized = store.authorized === false;
  checks.keyCleared = !storage.has("amkr.apiKey");
} else if (scenario === "no_key_prompts_login") {
  checks.loginCard = text().includes("连接到 AMKR");
  checks.unauthorized = store.authorized === false;
} else if (scenario === "valid_key_renders_page") {
  checks.noLoginCard = !text().includes("连接到 AMKR");
  checks.authorized = store.authorized === true;
  checks.pageRendered = text().includes("设置");
  checks.noAuthError = !text().includes("401");
} else if (scenario === "wrong_key_submit_shows_error") {
  await submitKey("bad-key");
  checks.errorShown = text().includes("无效");
  checks.stillLoginCard = text().includes("连接到 AMKR");
  checks.keyNotStored = !storage.has("amkr.apiKey");
  checks.noReload = reloads === 0;
} else if (scenario === "correct_key_submit_reloads") {
  await submitKey("good-key");
  checks.keyStored = storage.get("amkr.apiKey") === "good-key";
  checks.reloaded = reloads === 1;
  checks.authorized = store.authorized === true;
} else if (scenario === "mid_session_401_returns_to_login") {
  checks.startedAuthorized = store.authorized === true;
  // 模拟"重置本地鉴权 Key"：服务端换了 Key，浏览器里的旧 Key 立刻失效。
  server.accepted = new Set(["new-key"]);
  await api.settings().catch(() => {});
  checks.backToLogin = store.authorized === false && text().includes("连接到 AMKR");
  checks.reasonShown = text().includes("已失效");
} else if (scenario === "login_input_survives_health_poll") {
  const field = inputs()[0];
  checks.loginFieldPresent = Boolean(field);
  if (field) {
    field.value = "half-typed";
    renderShell(); // 健康轮询会整壳重绘
    checks.sameNode = inputs()[0] === field;
    checks.valueKept = inputs()[0]?.value === "half-typed";
  }
} else if (scenario === "auth_disabled_no_login") {
  checks.noLoginCard = !text().includes("连接到 AMKR");
  checks.authorized = store.authorized === true;
}

const failed = Object.entries(checks).filter(([, ok]) => !ok).map(([name]) => name);
console.log(JSON.stringify({ scenario, checks, failed }));
process.exit(failed.length ? 1 : 0);
