// 访客看板回归探针：用最小 DOM 垫片驱动 webui/pages/guest.js 与 guest-api.js。
// 用法：node webui/probes/webui_guest_probe.mjs <scenario>，结果以 JSON 打到 stdout。
//
// 为什么值得单独锁：访客看板是**唯一**把访问密钥交给浏览器并长期保存的地方，它的
// 失败模式都是安全性或"读数读反"的，而不是"页面不好看"：
//
//   1. 绝不能读管理面的 localStorage 键（amkr.apiKey）——共用键名会让访客的访问密钥
//      覆盖管理员已登录的本地鉴权 Key，管理员回到管理面就掉线；
//   2. 绝不能自己指定密钥或工作空间——可见范围必须完全由凭据决定，前端多发一个参数
//      就等于把"钉死"交给一个可以被改的输入；
//   3. 明文 key 绝不能出现在页面上——这一页会被投屏、截图、随手分享；
//   4. 三态错误必须分开：401（凭据无效，请重填）与 403（已被停用，去找管理员）要给出
//      不同的指引，混成一条会让人白找半天。
//
// 每个场景单独起进程：guest.js 的 state 是模块级缓存，同进程连跑会互相污染。

import { pathToFileURL } from "node:url";
import path from "node:path";

const WEBUI = path.resolve(import.meta.dirname, "..");
const scenario = process.argv[2];

// —— 最小 DOM 垫片（与 webui_panel_probe.mjs 同一套）——
class FakeNode {
  constructor(tag) {
    this.tagName = tag;
    this.children = [];
    this.attrs = {};
    this.listeners = {};
    this.className = "";
    this.style = {};
    this.value = "";
    this.disabled = false;
    this.parent = null;
    this.documentRoot = false;
  }
  append(...nodes) {
    for (const node of nodes.flat()) {
      if (node === null || node === undefined) continue;
      if (typeof node === "object") node.parent = this;
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
  select() {}
  get isConnected() {
    for (let node = this; node; node = node.parent) if (node.documentRoot) return true;
    return false;
  }
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
root.documentRoot = true;

const define = (name, value) =>
  Object.defineProperty(global, name, { value, writable: true, configurable: true });

const location = {
  hash: "",
  pathname: "/ui/guest.html",
  href: "http://127.0.0.1:28881/ui/guest.html",
};
define("location", location);
define("window", { addEventListener() {}, isSecureContext: true, location, confirm: () => true });
define("navigator", { clipboard: null });
define("history", { replaceState() {} });
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

define("setInterval", () => 0);
define("clearInterval", () => {});
define("setTimeout", () => 0);

// localStorage：**可用**，但要记录每一次访问，好断言键名没撞上管理面。
const storageTouches = [];
const store = new Map();
define("localStorage", {
  getItem: (k) => { storageTouches.push(`get:${k}`); return store.has(k) ? store.get(k) : null; },
  setItem: (k, v) => { storageTouches.push(`set:${k}`); store.set(k, v); },
  removeItem: (k) => { storageTouches.push(`remove:${k}`); store.delete(k); },
  clear: () => { storageTouches.push("clear"); store.clear(); },
});

// —— 假服务端 ——
const server = {
  guestKey: "amkr_ak_trial",
  // status 决定这个 key 的命运：200 正常 / 401 无效 / 403 停用。
  status: 200,
  requests: [],
  // 管理员面已登录时 local 里会有这个键；探针断言访客**从不**碰它。
  adminKey: "local-admin-key",
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
  server.requests.push({
    url,
    method: options.method || "GET",
    bearer,
    hadAuthHeader: "Authorization" in headers,
    workspaceHeader: headers["X-AMKR-Workspace"] ?? null,
  });

  const path = url.split("?")[0];
  // 价格目录不鉴权：即使 key 无效也要能取到（它只是公开的价格数据）。
  if (path === "/ui/pricing.json") {
    return respond(200, {
      updated_at: "2026-01-01T00:00:00Z",
      models: { "model-a": { input: 3, output: 15 }, "model-b": { input: 1, output: 2 } },
    });
  }
  if (path === "/ui/access-key-usage.json") {
    if (server.status !== 200) {
      return respond(server.status, { error: { message: "本地 API key 验证失败" } });
    }
    if (bearer !== server.guestKey) {
      return respond(401, { error: { message: "本地 API key 验证失败" } });
    }
    return respond(200, {
      count_semantics: "upstream_attempt",
      window: { from: "2026-01-01T00:00:00+08:00", to: "2026-01-02T00:00:00+08:00", hours: 24 },
      access_key_id: "trial",
      access_key_name: "试用账号 A",
      stats: {
        requests: 4, successes: 3, failures: 1, retries: 1,
        prompt_tokens: 1000, completion_tokens: 500, total_tokens: 1500, cached_tokens: 0,
        avg_duration_ms: 120,
      },
      dimensions: {
        model_id: { "model-a": { requests: 4, total_tokens: 1500 } },
        provider_id: { "prov-a": { requests: 4, total_tokens: 1500 } },
        upstream_model_id: { "model-a": { requests: 4, total_tokens: 1500, prompt_tokens: 1000, completion_tokens: 500 } },
      },
      recent_requests: [
        {
          created_at: "2026-01-01T10:00:00+08:00", model_id: "model-a", provider_id: "prov-a",
          upstream_model_id: "model-a", status_code: 200, success: true, retried: false,
          prompt_tokens: 100, completion_tokens: 50, total_tokens: 150, cached_tokens: 0,
          first_token_ms: 20, duration_ms: 120,
        },
        {
          created_at: "2026-01-01T09:00:00+08:00", model_id: "model-a", provider_id: null,
          upstream_model_id: null, status_code: null, success: false, retried: true,
          prompt_tokens: 0, completion_tokens: 0, total_tokens: 0, cached_tokens: 0,
          first_token_ms: 0, duration_ms: 0,
        },
      ],
    });
  }
  return respond(404, { error: { message: "Not Found" } });
};

// —— 驱动 ——
const { bootGuest } = await import(pathToFileURL(path.join(WEBUI, "pages", "guest.js")).href);

const findAll = (node, predicate, out = []) => {
  if (predicate(node)) out.push(node);
  for (const child of node.children) if (typeof child === "object") findAll(child, predicate, out);
  return out;
};
const text = () => root.textContent;
const inputs = () => findAll(root, (n) => n.tagName === "input");
const buttons = () => findAll(root, (n) => n.tagName === "button");
const clickButton = async (label) => {
  const target = buttons().find((b) => b.textContent.trim() === label);
  if (!target) throw new Error(`找不到按钮: ${label}（页面文字：${text().slice(0, 300)}）`);
  for (const handler of target.listeners.click || []) await handler({});
};
const settle = async () => { for (let i = 0; i < 20; i += 1) await Promise.resolve(); };

const setup = {
  // 没有 key：必须请人填，而不是直接打接口（否则用户看到 401，以为 key 错了）。
  no_key_asks_for_one: () => {
    store.set("amkr.apiKey", server.adminKey);
  },
  // 已保存 key：应立刻取数，并显示密钥**名字**（来自服务端配置）而不是明文 key。
  saved_key_loads_dashboard: () => {
    store.set("amkr.apiKey", server.adminKey);
    store.set("amkr.guestAccessKey", server.guestKey);
  },
  // 错的 key：必须说"无效"并重新给出填写入口，而不是"服务故障"。
  wrong_key_says_invalid: () => {
    store.set("amkr.guestAccessKey", "amkr_ak_wrong");
  },
  // 被停用的 key：403，文案要指向管理员，而不是让人反复核对 key。
  disabled_key_points_to_admin: () => {
    server.status = 403;
    store.set("amkr.guestAccessKey", server.guestKey);
  },
  // 面板读数里的 dimensions 键是原始列名，页面必须把它们翻成中文榜。
  shows_rankings_and_costs: () => {
    store.set("amkr.guestAccessKey", server.guestKey);
  },
};
const scenarioNames = Object.keys(setup);
if (scenario === undefined) {
  const { spawnSync } = await import("node:child_process");
  let failedRuns = 0;
  for (const name of scenarioNames) {
    process.stdout.write(`--- ${name}\n`);
    const result = spawnSync(process.execPath, [process.argv[1], name], { stdio: "inherit" });
    if (result.status !== 0) failedRuns += 1;
  }
  process.exit(failedRuns ? 1 : 0);
}
if (!scenarioNames.includes(scenario)) {
  console.error(`未知场景：${scenario}\n可用场景：\n  ${scenarioNames.join("\n  ")}`);
  process.exit(2);
}
setup[scenario]();

bootGuest();
await settle();

const checks = {};

// 所有场景都必须成立的两条硬约束。
const commonChecks = () => {
  // 1. **从不**读管理面的键。共用键名会让访客的访问密钥覆盖管理员的本地鉴权 Key。
  checks.neverReadsAdminKey = !storageTouches.some((t) => t.includes("amkr.apiKey"));
  // 2. **从不**自己指定工作空间：可见范围完全由凭据决定。
  checks.neverSendsWorkspaceHeader = server.requests.every((r) => r.workspaceHeader === null);
  // 3. **从不**发写请求：访客看板是只读的。
  checks.onlyGetRequests = server.requests.every((r) => r.method === "GET");
  // 4. 明文 key 绝不出现在页面上。
  checks.neverShowsPlaintextKey = !text().includes(server.guestKey);
};

if (scenario === "no_key_asks_for_one") {
  checks.promptsForKey = text().includes("访问密钥");
  checks.hasKeyField = inputs().some((n) => n.attrs.type === "password");
  // 关键：**没有**发过任何读数请求。直接打接口会让用户看到 401，误以为 key 错了。
  checks.madeNoUsageRequests = !server.requests.some((r) => r.url.includes("access-key-usage"));
  commonChecks();
} else if (scenario === "saved_key_loads_dashboard") {
  checks.requestedUsage = server.requests.some((r) => r.url.startsWith("/ui/access-key-usage.json"));
  // 凭据走 Authorization: Bearer，且用的正是访客自己那把。
  checks.sentKeyAsBearer = server.requests
    .filter((r) => r.url.includes("access-key-usage"))
    .every((r) => r.bearer === server.guestKey);
  // 显示服务端给的**名字**（来自配置），不是从 key 猜出来的。
  checks.showsKeyName = text().includes("试用账号 A");
  checks.showsKeyTail = text().includes("trial");
  checks.showsRequests = text().includes("4");
  checks.showsSuccessRate = text().includes("成功");
  // 三个维度都要有成中文标题的榜。
  checks.showsModelRanking = text().includes("模型");
  checks.showsProviderRanking = text().includes("供应商");
  // 最近调用明细的两条都要在：一条成功、一条无响应（status_code 为 null）。
  checks.showsRecentCalls = text().includes("最近调用") && text().includes("model-a");
  checks.showsNoResponseStatus = text().includes("无响应");
  // 重试过的请求要有记号（排障第一个要看的东西）。
  checks.marksRetried = text().includes("重试");
  // 成本：价格目录已给，model-a 有价（input 3 / output 15），因此应出现估算金额。
  checks.showsCost = text().includes("花费估算") && text().includes("$");
  // 且**花费卡里的每根条形**都要有金额，不能只有页脚合计有。
  // 这一条抓的是 barList 的 format 契约：它收到的是**整行**而不是 value
  // （charts.js:443 的 format(row)），写成 (value) => formatCost(value) 会让每根条都
  // 显示 "—"，而页脚合计仍然正确——只断言 text().includes("$") 是抓不到的。
  //
  // 必须**限定在花费卡内**：模型/供应商两张排行卡也用 barList，它们的值格式是
  // "N 次"，一起统计会让断言永远为假（这正是第一版的写法，永远失败）。
  const costCard = findAll(root, (n) => (n.className || "") === "card"
    && n.textContent.includes("花费估算（按上游模型）"))[0];
  const costBars = costCard
    ? findAll(costCard, (n) => (n.className || "").includes("bar-value"))
    : [];
  checks.costBarsShowMoney = costBars.length > 0
    && costBars.every((node) => node.textContent.includes("$"));
  // 只读：不碰管理面的 api.js 那套 localStorage 键（上面 commonChecks 已断）。
  commonChecks();
} else if (scenario === "wrong_key_says_invalid") {
  checks.saysKeyInvalid = text().includes("无效");
  // 必须重新给出填写入口——只说失败会让人无处可去。
  checks.offersKeyField = inputs().some((n) => n.attrs.type === "password");
  checks.triedTheKey = server.requests
    .filter((r) => r.url.includes("access-key-usage"))
    .every((r) => r.bearer === "amkr_ak_wrong");
  commonChecks();
} else if (scenario === "disabled_key_points_to_admin") {
  // 403 的文案必须指向管理员，而**不是**"无效，请重填"——重填还是同一把 key。
  checks.saysDisabled = text().includes("停用");
  checks.pointsToAdmin = text().includes("管理员");
  checks.doesNotSayInvalid = !text().includes("无效");
  commonChecks();
} else if (scenario === "shows_rankings_and_costs") {
  // dimensions 的键是原始列名（model_id / provider_id / upstream_model_id），页面必须
  // 翻成中文标题，而不是把列名直接显示出来。
  checks.translatesDimensions = text().includes("模型") && text().includes("供应商")
    && text().includes("上游模型");
  checks.doesNotLeakColumnNames = !text().includes("model_id") && !text().includes("provider_id");
  commonChecks();
}

const failed = Object.entries(checks).filter(([, ok]) => !ok).map(([name]) => name);
console.log(JSON.stringify({ scenario, checks, failed }));
process.exit(failed.length ? 1 : 0);
