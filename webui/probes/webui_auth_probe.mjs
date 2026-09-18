// WebUI 鉴权流程回归探针：用最小 DOM 垫片驱动真实的 webui ES 模块。
// 用法：node webui_auth_probe.mjs <scenario>，结果以 JSON 打到 stdout。
// 每个场景单独起进程，保证模块级缓存（各页面的 state）互不干扰。

import { pathToFileURL } from "node:url";
import path from "node:path";

const WEBUI = path.resolve(import.meta.dirname, "..");
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
  // 忠实还原 isConnected：沿 parent 走到文档根才算已挂载。页面（概览/活动）用它
  // 跳过"离开后仍在途的重绘"，垫片若恒为 true/false 都会把这个判断测成另一回事。
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

// Node 24 已内置只读的 navigator/location 全局，只能用 defineProperty 覆盖。
const define = (name, value) =>
  Object.defineProperty(global, name, { value, writable: true, configurable: true });

let reloads = 0;
define("location", {
  hash: "#/settings",
  // 独立运行时 WebUI 在 /ui/ 下；嵌入场景会覆盖成 /<prefix>/ui/。
  pathname: "/ui/",
  reload() { reloads += 1; },
});
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
  // 嵌入宿主时的挂载前缀；独立运行是空串。
  prefix: "",
  // 任务路由探针用的假数据与写入记录。
  tasks: [],
  writes: [],
  // 成本页探针用：价格目录（null = 服务端尚未就绪，应回 503）与该窗口的上游用量。
  pricing: null,
  pricingStatus: 200,
  upstreamModels: {},
  providers: {},
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

  // 前缀必须体现在真实请求上：这里剥掉前缀再匹配，未加前缀的请求会落到 401。
  const path = server.prefix && url.startsWith(server.prefix)
    ? url.slice(server.prefix.length)
    : url;

  if (server.offline) throw new TypeError("fetch failed");
  if (path === "/health") {
    return respond(200, {
      status: "ok",
      version: "4.0.3",
      models: [],
      local_auth_enabled: server.authEnabled,
    });
  }
  // 价格目录（models.dev）：由服务端缓存后挂在 WebUI 前缀下，且**不鉴权**——它是
  // models.dev 的公开数据。因此必须排在**鉴权分支之前**：放到后面就永远拿不到，
  // 而未鉴权时拿不到目录正是成本列该显示 "—" 的场景之一。
  if (path === "/ui/pricing.json") {
    if (!server.pricing) return respond(server.pricingStatus, { detail: "价格目录尚不可用" });
    return respond(200, server.pricing);
  }
  if (server.authEnabled && !server.accepted.has(bearer)) {
    return respond(401, { detail: "本地 API key 验证失败" });
  }
  if (path.startsWith("/metrics")) {
    // 概览页的两条读取：窗口快照与时间序列。给最小但结构完整的载荷，
    // 让首页能真正画出 KPI 瓦片与两张图（否则测的就不是"重绘"而是"空态"）。
    if (path.startsWith("/metrics/series")) {
      return respond(200, {
        bucket_seconds: 15,
        points: [{ started_at: "2026-01-01T10:00:00+08:00", ended_at: "2026-01-01T10:00:15+08:00", complete: true, requests: 3, successes: 3, failures: 0, retries: 0, prompt_tokens: 30, completion_tokens: 10, total_tokens: 40, cached_tokens: 0, total_duration_ms: 300, total_first_token_ms: 100 }],
      });
    }
    return respond(200, {
      count_semantics: "attempts",
      window: { from: null, to: "2026-01-01T10:00:00+08:00", hours: 1 },
      rate_window_seconds: 60,
      current_rpm: 3,
      current_tpm: 40,
      router_status: "green",
      active_requests: 0,
      total: { requests: 3, successes: 3, failures: 0, retries: 0, prompt_tokens: 30, completion_tokens: 10, total_tokens: 40, cached_tokens: 0 },
      caller_types: {},
      models: {},
      providers: server.providers,
      upstream_models: server.upstreamModels,
      unattributed: {},
    });
  }

  if (path.startsWith("/api/settings")) {
    return respond(200, {
      config_revision: "rev-1",
      settings: { host: "127.0.0.1", port: 28881, max_retries: 2, local_auth_enabled: true },
    });
  }
  if (path.startsWith("/api/models")) {
    return respond(200, {
      config_revision: "rev-1",
      models: [
        { id: "model-a", aliases: [], keys: [], routing_mode: "round_robin" },
        { id: "model-b", aliases: [], keys: [], routing_mode: "round_robin" },
      ],
    });
  }
  if (path.startsWith("/api/tasks")) {
    if (options.method === "POST" || options.method === "PUT") {
      server.writes.push(JSON.parse(options.body));
      // 需要观察"保存进行中"的状态时，把响应挂住，由用例自己放行。
      if (server.holdSave) return new Promise((resolve) => { server.releaseSave = () => resolve(respond(200, { config_revision: "rev-2" })); });
      return respond(options.method === "POST" ? 201 : 200, { config_revision: "rev-2" });
    }
    return respond(200, {
      config_revision: "rev-1",
      tasks: server.tasks,
    });
  }
  return respond(200, {});
};

// —— 驱动 ——
const { boot, store, renderShell, navigate } = await import(pathToFileURL(path.join(WEBUI, "app.js")).href);
const { api } = await import(pathToFileURL(path.join(WEBUI, "api.js")).href);

function findAll(node, predicate, out = []) {
  if (predicate(node)) out.push(node);
  for (const child of node.children) findAll(child, predicate, out);
  return out;
}

const text = () => root.textContent;
const inputs = () => findAll(root, (n) => n.tagName === "input");
const buttons = () => findAll(root, (n) => n.tagName === "button");
const byClass = (name) => findAll(root, (n) => String(n.className || "").split(/\s+/).includes(name));
const clickButton = async (label) => {
  const target = buttons().find((b) => b.textContent.trim() === label);
  if (!target) throw new Error(`找不到按钮: ${label}`);
  for (const handler of target.listeners.click || []) await handler({});
};
const submitKey = async (value) => {
  const field = inputs()[0];
  if (!field) throw new Error("验证页没有 Key 输入框");
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
  // 深链进入：地址栏直接指向某个内页，未鉴权时不能绕过验证页。
  deeplink_without_key_stays_on_login: () => {
    global.location.hash = "#/providers";
  },
  unreachable_service_stays_on_login: () => {
    server.offline = true;
    storage.set("amkr.apiKey", "good-key");
  },
  // 嵌入宿主：页面位于 /amkr/ui/，API 必须打到 /amkr 下而不是根路径。
  mounted_prefix_uses_prefixed_api: () => {
    server.prefix = "/amkr";
    global.location.pathname = "/amkr/ui/";
    storage.set("amkr.apiKey", "good-key");
  },
  // 任务路由页：直接驱动真实页面模块，锁住表单与请求体形状。
  tasks_page_lists_and_saves: () => {
    global.location.hash = "#/tasks";
    storage.set("amkr.apiKey", "good-key");
    server.tasks = [
      {
        name: "TASK_000001",
        model: "model-a",
        fallback_model: null,
        params: { temperature: 0.2, reasoning_effort: "high" },
      },
    ];
  },
  // 任务路由页「新建」：空列表下点新建必须画出编辑器。taskEditor(null) 走的是
  // 与编辑不同的分支，任何一处对 task 直接取属性都会抛错，表现为点了没反应。
  tasks_new_task_opens_editor: () => {
    global.location.hash = "#/tasks";
    storage.set("amkr.apiKey", "good-key");
    server.tasks = [];
  },
  // 成本页（有价格目录）：KPI、排行与逐条成本都必须画出来。
  // 目录与用量都给真实形状，这样断言的是"算出来的钱对不对"，而不是"页面没崩"。
  cost_page_renders_with_pricing: () => {
    global.location.hash = "#/cost";
    storage.set("amkr.apiKey", "good-key");
    server.pricing = {
      version: 1,
      source: "https://models.dev/api.json",
      updated_at: "2026-01-02T03:04:05Z",
      error: null,
      models: { "gpt-4o": { input: 2.5, output: 10, cache_read: 1.25 } },
    };
    server.upstreamModels = {
      "gpt-4o": { requests: 2, successes: 2, failures: 0, retries: 0, prompt_tokens: 1000000, completion_tokens: 0, total_tokens: 1000000, cached_tokens: 0, cache_read_input_tokens: 0, cache_creation_input_tokens: 0 },
    };
  },
  // 成本页（目录可用，但该上游模型**不在**目录里）：这是"无定价"最真实的样子——
  // 目录是好的，只是没这个模型的价。此处必须显示 "—"，且绝不能退化成 $0。
  cost_page_unmatched_model_shows_dash: () => {
    global.location.hash = "#/cost";
    storage.set("amkr.apiKey", "good-key");
    server.pricing = {
      version: 1,
      source: "https://models.dev/api.json",
      updated_at: "2026-01-02T03:04:05Z",
      error: null,
      models: { "gpt-4o": { input: 2.5, output: 10, cache_read: 1.25 } },
    };
    // 名字与目录里任何一条都对不上（且没有日期后缀可剥）。
    server.upstreamModels = {
      "totally-unknown-model-xyz": { requests: 2, successes: 2, failures: 0, retries: 0, prompt_tokens: 1000000, completion_tokens: 500000, total_tokens: 1500000, cached_tokens: 0 },
    };
  },
  // 成本页（目录尚未就绪 → 服务端 503）：必须显示"无定价"，**绝不能**显示 $0。
  // 这是整条链路最要命的失败模式：把"不知道"渲染成"免费"。
  cost_page_without_pricing_shows_dash: () => {
    global.location.hash = "#/cost";
    storage.set("amkr.apiKey", "good-key");
    server.pricing = null;
    server.pricingStatus = 503;
    server.upstreamModels = {
      "gpt-4o": { requests: 2, successes: 2, failures: 0, retries: 0, prompt_tokens: 1000, completion_tokens: 100, total_tokens: 1100, cached_tokens: 0 },
    };
  },
  // 概览页二次进入：模块级 state 已有缓存时，进入必须**同步**画出内容。
  // 曾经的缺陷：renderOverview 先建好尚未挂载的 host 再调 draw()，而 draw() 用
  // isConnected 守卫挡住了这次同步首绘；同时 loadWindow/loadHeatmap 命中缓存后
  // 直接返回、不会再回调 draw —— 于是切回概览整页空白，一直等到下一次轮询
  // （健康轮询 5 秒）才补画，用户看到的就是"进入会卡顿一段时间"。
  overview_reentry_paints_immediately: () => {
    global.location.hash = "#/overview";
    storage.set("amkr.apiKey", "good-key");
  },
};
// 没给场景名时，把自己按场景逐个重跑一遍。两个理由：
//   1) 各页面模块的 state 是模块级缓存，同进程连跑多个场景会互相污染，必须一场景一进程；
//   2) 不传场景名时 setup 不会命中，一个断言都不跑、failed 为空，进程照样退出 0。
//      CI 与 README 都是裸调这条探针的，于是它成了永远绿灯的空转——"点新建没反应"
//      这类只在某个分支上出现的 bug，正好从这种空子里漏过去。默认跑全部才不漏。
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
// 场景名打错同样会静默空转，必须响亮地失败而不是"什么都没检查却通过"。
if (!scenarioNames.includes(scenario)) {
  console.error(`未知场景：${scenario}\n可用场景：\n  ${scenarioNames.join("\n  ")}`);
  process.exit(2);
}
setup[scenario]();

await boot();
// 页面首屏的读取是异步的，且可能会渲染不止一次；这里把在途的微任务排空，让断言
// 看到的是稳定后的页面（否则断言的就是"恰好还没画完"的中间态）。
const settle = async () => { for (let i = 0; i < 20; i += 1) await Promise.resolve(); };
await settle();

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
  // 独立页：不能带应用栏与导航，未鉴权时无从"进入"主界面。
  checks.noAppBar = byClass("app-bar").length === 0;
  checks.noNav = byClass("nav").length === 0;
  checks.noShell = byClass("shell").length === 0;
  checks.noPages = !text().includes("模型路由") && !text().includes("统一模型");
  checks.fullHeight = byClass("login-shell").length === 1;
} else if (scenario === "deeplink_without_key_stays_on_login") {
  // 地址栏直达内页也必须先过验证页。
  checks.pageWasProviders = store.page === "providers";
  checks.stillLogin = store.authorized === false && text().includes("连接到 AMKR");
  checks.noProvidersPage = !text().includes("供应商");
  checks.noNav = byClass("nav").length === 0;
} else if (scenario === "unreachable_service_stays_on_login") {
  // 连不上时无从判断是否需要鉴权：宁可停在验证页，也不拿没验证过的状态进主界面。
  checks.stillLogin = store.authorized === false && text().includes("连接到 AMKR");
  checks.reasonShown = text().includes("无法连接");
  checks.keyKept = storage.get("amkr.apiKey") === "good-key";
  checks.retryOffered = buttons().some((b) => b.textContent.includes("重试连接"));
} else if (scenario === "mounted_prefix_uses_prefixed_api") {
  // 嵌入宿主时页面在 /amkr/ui/ 下，若 API 基址写死绝对路径，每个请求都会打到宿主
  // 根路径（404/401），页面直接停在"读取设置失败"。
  checks.authorized = store.authorized === true;
  checks.pageRendered = text().includes("设置");
  checks.requestsArePrefixed = server.requests.length > 0
    && server.requests.every((r) => String(r.url).startsWith("/amkr"));
  checks.healthPrefixed = server.requests.some((r) => r.url === "/amkr/health");
  checks.settingsPrefixed = server.requests.some((r) => r.url === "/amkr/api/settings");
} else if (scenario === "valid_key_renders_page") {
  checks.noLoginCard = !text().includes("连接到 AMKR");
  checks.authorized = store.authorized === true;
  checks.pageRendered = text().includes("设置");
  checks.noAuthError = !text().includes("401");
  checks.hasAppBar = byClass("app-bar").length === 1;
  checks.hasNav = byClass("nav").length === 1;
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
  checks.pageRendered = text().includes("设置");
  checks.hasAppBar = byClass("app-bar").length === 1;
} else if (scenario === "tasks_page_lists_and_saves") {
  checks.authorized = store.authorized === true;
  checks.onTasksPage = store.page === "tasks";
  // 已有任务要列出来，并显示它的固定参数。
  checks.listsTask = text().includes("TASK_000001");
  checks.showsParams = text().includes("temperature") && text().includes("reasoning_effort");

  // 打开编辑器，改一个固定参数并保存。
  await clickButton("编辑");
  checks.explainsRejection = text().includes("会被直接拒绝");
  checks.hasAllParamFields = inputs().filter((node) => node.attrs.placeholder === "留空表示不固定").length === 6;
  const taskInput = inputs().find((node) => node.value === "TASK_000001");
  // 编辑时任务名不可改（它是调用方用的 model 名，改名等于换了个任务）。
  checks.taskNameReadOnly = Boolean(taskInput) && taskInput.disabled === true;
  const tempInput = inputs().find((node) => node.attrs.placeholder === "留空表示不固定");
  checks.tempPrefilled = tempInput?.value === "0.2";
  if (tempInput) tempInput.value = "0.7";

  // 切首选模型不能重建表单：重建会把手填的参数一起清掉。这里先填一个只在内存里的
  // 值，再切模型，确认那个输入框还是同一个节点、值还在。
  const selects = findAll(root, (n) => n.tagName === "select");
  const primarySelect = selects[0];
  checks.primarySelectListsAllModels = primarySelect?.children.length === 2;
  if (primarySelect) {
    primarySelect.value = "model-b";
    for (const handler of primarySelect.listeners.change || []) await handler({ target: primarySelect });
  }
  checks.survivesModelChange = inputs().find((node) => node.attrs.placeholder === "留空表示不固定") === tempInput;
  checks.valueKeptOnModelChange = tempInput?.value === "0.7";
  // 备选下拉必须排掉当前首选，否则会存出「首选 == 备选」的任务。
  checks.fallbackExcludesPrimary = selects[1]?.children.every((option) => option.attrs.value !== "model-b");

  // 保存期间表单要锁住：请求已经在路上，此时让用户继续改只会造成"改了却没生效"。
  // 这里把响应挂住，趁机检查锁定状态，再放行。
  server.holdSave = true;
  const saving = clickButton("保存任务");
  await settle();
  checks.nameStillReadOnly = inputs().find((node) => node.value === "TASK_000001")?.disabled === true;
  // 输入框、两个模型下拉、以及编辑器自己的两个按钮都要锁住（导航按钮不参与）。
  checks.formLockedWhileSaving = inputs().every((node) => node.disabled === true)
    && selects.every((node) => node.disabled === true)
    && ["保存中…", "取消"].every((label) =>
      buttons().find((node) => node.textContent.trim() === label)?.disabled === true);
  checks.saveButtonShowsProgress = buttons().some((node) => node.textContent.includes("保存中"));
  server.holdSave = false;
  server.releaseSave();
  await saving;
  await settle();

  const write = server.writes.at(-1);
  checks.wroteTask = Boolean(write);
  // 保存走的是 PUT /api/tasks/<name>（新建才用 POST + 任务名放 body）。
  checks.wroteUpdateUrl = server.requests.some((r) => r.url === "/api/tasks/TASK_000001");
  checks.writeHasModel = write?.model === "model-b";
  checks.writeHasNewTemperature = write?.params?.temperature === 0.7;
  checks.writeKeptEffort = write?.params?.reasoning_effort === "high";
  // 留空的参数不该被写成 null 塞进配置。
  checks.writeOmitsBlankParams = write !== undefined && !("top_p" in (write.params || {}));
  // 保存成功后编辑器关闭，表单不再锁着。
  checks.editorClosedAfterSave = !buttons().some((node) => node.textContent.trim() === "保存任务");
} else if (scenario === "tasks_new_task_opens_editor") {
  checks.authorized = store.authorized === true;
  checks.onTasksPage = store.page === "tasks";
  // 空列表时要给出空态，并且空态自带一个新建入口。
  checks.showsEmptyState = text().includes("尚未配置任务路由");

  // 点「新建任务」必须真的画出编辑器：这里若抛错，draw() 中断，界面停在原样，
  // 用户看到的就是"点了没反应"。
  await clickButton("新建任务");
  checks.editorOpened = text().includes("固定采样参数");
  checks.hasSaveButton = buttons().some((node) => node.textContent.trim() === "保存任务");

  // 新建时任务名可填（编辑既有任务时才只读）。
  const nameInput = inputs().find((node) => node.attrs.placeholder === "TASK_000001");
  checks.hasNameField = Boolean(nameInput) && nameInput.disabled !== true;
  // 6 个数值参数 + stop，全部为空且可编辑。
  checks.hasAllParamFields = inputs().filter((node) => node.attrs.placeholder === "留空表示不固定").length === 6;
  checks.hasStopField = inputs().some((node) => node.attrs.placeholder === "逗号分隔，留空表示不固定");

  // 填名字后保存：新建走 POST，任务名放在 body 里。
  if (nameInput) nameInput.value = "TASK_000002";
  await clickButton("保存任务");
  await settle();

  const write = server.writes.at(-1);
  checks.wroteCreate = Boolean(write);
  checks.wroteCreateUrl = server.requests.some((r) => r.url === "/api/tasks");
  checks.writeHasName = write?.name === "TASK_000002";
  checks.writeHasModel = write?.model === "model-a";
  // 没填的参数不该被写进配置。
  checks.writeOmitsBlankParams = write !== undefined && !("temperature" in (write.params || {}));
} else if (scenario === "cost_page_renders_with_pricing") {
  await settle();
  // 页面骨架与四张卡都在。
  checks.hasStatGrid = byClass("stat-grid").length === 1;
  checks.hasCostList = byClass("cost-row").length === 0 || byClass("cost-list").length === 1;
  const body = text();
  // 1M 输入 token × $2.5/1M = $2.50：金额必须真的算出来，而不是只画了壳。
  checks.showsComputedAmount = body.includes("$2.50");
  checks.showsCoverage = body.includes("计价覆盖率");
  checks.showsPriceDetail = body.includes("$2.5");
  // 中文标题确认渲染的是成本页而不是别的页。
  checks.isCostPage = body.includes("成本") && body.includes("models.dev");
  // 目录已就绪时不应出现"无定价"降级提示。
  checks.noUnavailableNotice = !body.includes("价格目录尚不可用");
  checks.requestedPricing = server.requests.some((r) => r.url === "/ui/pricing.json");
} else if (scenario === "cost_page_unmatched_model_shows_dash") {
  await settle();
  const body = text();
  // 目录是好的，所以**不该**出现"目录尚不可用"；该出现的是"没匹配到单价"。
  checks.noUnavailableNotice = !body.includes("价格目录尚不可用");
  checks.showsUnmatched = body.includes("没有匹配到单价") || body.includes("无定价") || body.includes("未匹配");
  // 关键：1.5M token 的用量在没有任何单价时**绝不能**变成 $0。
  checks.neverShowsZeroDollars = !body.includes("$0");
  // 覆盖率必须显示为 0%，把"一分钱都没算进来"讲明白。
  checks.showsZeroCoverage = body.includes("0%");
  checks.requestedPricing = server.requests.some((r) => r.url === "/ui/pricing.json");
} else if (scenario === "cost_page_without_pricing_shows_dash") {
  await settle();
  const body = text();
  // 目录不可用 → 明确说明，且**任何位置都不出现 $0**。
  checks.showsUnavailable = body.includes("无定价") || body.includes("价格目录尚不可用");
  checks.neverShowsZeroDollars = !body.includes("$0");
  // 用量仍然可见（页面降级但不空转）。
  checks.stillShowsTokens = body.includes("Token 用量");
  // 即便目录拿不到，也必须真的去请求过（否则"没显示 $0"只是因为压根没刷新）。
  checks.requestedPricing = server.requests.some((r) => r.url === "/ui/pricing.json");
} else if (scenario === "overview_reentry_paints_immediately") {
  // 首次进入：等异步数据落地，确认首页确实画出了 KPI 瓦片。
  await settle();
  checks.firstEntryHasContent = byClass("stat-grid").length === 1;

  // 切走再切回：这一次 state 里的快照/序列/热力图都还在缓存里，两条 load* 都会
  // 提前 return（不产生任何回调），所以内容只能靠 renderOverview 里的同步首绘。
  navigate("settings");
  await settle();
  checks.leftOverview = store.page === "settings" && byClass("stat-grid").length === 0;

  navigate("overview");
  // 刻意**不**排空微任务：切回后必须当场就有内容，而不是等下一次轮询。
  checks.reentryPaintsSynchronously = byClass("stat-grid").length === 1;
  checks.reentryHasHeatmap = byClass("heat-cell").length > 0;
  // 用 .chart-host（折线 + 堆叠柱各一）而不是数 <svg>：导航图标也是 svg，
  // 数 svg 在"页面空白只剩余壳"时照样为真，那种断言等于没测。
  checks.reentryHasCharts = byClass("chart-host").length === 2;
}

const failed = Object.entries(checks).filter(([, ok]) => !ok).map(([name]) => name);
console.log(JSON.stringify({ scenario, checks, failed }));
process.exit(failed.length ? 1 : 0);
