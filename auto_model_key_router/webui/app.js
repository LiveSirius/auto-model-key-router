// WebUI 外壳：连接状态、鉴权、导航与全局轮询。
// 轮询节奏对齐 Keyloom：健康 5s、指标 15s（活动页 2s）。

import { h, mount, errorText } from "./dom.js";
import { api, setKey, ApiError, onUnauthorized } from "./api.js";
import { installToastHost, notice, buttonNode, input, field, empty, render } from "./ui.js";

import { renderOverview } from "./pages/overview.js";
import { renderActivity } from "./pages/activity.js";
import { renderProviders } from "./pages/providers.js";
import { renderRouting } from "./pages/routing.js";
import { renderUnified } from "./pages/unified.js";
import { renderIntegrations } from "./pages/integrations.js";
import { renderSettings } from "./pages/settings.js";

export const PAGES = [
  { group: "工作台", items: [
    { id: "overview", label: "概览", icon: "◎", render: renderOverview },
    { id: "activity", label: "活动", icon: "≣", render: renderActivity },
  ]},
  { group: "配置", items: [
    { id: "providers", label: "供应商", icon: "⛁", render: renderProviders },
    { id: "routing", label: "模型路由", icon: "⇄", render: renderRouting },
    { id: "unified", label: "统一模型", icon: "★", render: renderUnified },
    { id: "integrations", label: "集成", icon: "⌘", render: renderIntegrations },
  ]},
  { group: "系统", items: [{ id: "settings", label: "设置", icon: "⚙", render: renderSettings }] },
];

export const store = {
  page: location.hash.replace(/^#\/?/, "") || "overview",
  health: null,
  metrics: null,
  metricsError: null,
  connectionError: null,
  authorized: false,
  revision: null,
};

let root = null;      // #root
let appHost = null;   // 应用内容：验证页或（应用栏 + 导航 + 内容）
let contentHost = null;
let healthTimer = null;
let metricsTimer = null;

export function currentPage() {
  for (const group of PAGES) {
    const found = group.items.find((item) => item.id === store.page);
    if (found) return found;
  }
  return PAGES[0].items[0];
}

// 深链（#/providers）与浏览器前进后退都走这里，未知页回落到首页。
function pageFromHash() {
  return location.hash.replace(/^#\/?/, "") || "overview";
}

export function navigate(page) {
  store.page = page;
  location.hash = `/${page}`;
  renderShell();
}

// 页面通过 ctx 触发局部重绘，避免整壳重建导致输入框失焦。
export function ctx() {
  return {
    store,
    navigate,
    rerender: renderContent,
    refreshHealth: loadHealth,
    refreshMetrics: () => loadMetrics(true),
    askLogin,
  };
}

async function fetchHealth() {
  try {
    store.health = await api.health();
    store.connectionError = null;
  } catch (error) {
    store.health = null;
    store.connectionError = errorText(error);
  }
}

async function loadHealth() {
  await fetchHealth();
  // 验证页的节点是复用的，重绘不会丢已粘贴一半的 Key 与焦点；服务恢复后提示也会跟着更新。
  renderShell();
}

// localStorage 里有 Key 不代表 Key 可用（可能已被重置或来自旧版本），必须实际请求一次。
// 返回值区分"未授权"与"连不上"，前者才清掉本地 Key，避免网络抖动误删有效 Key。
async function verifyAccess() {
  // 连不上时无从判断是否需要鉴权，一律当作未通过：宁可停在验证页，也不拿没验证过的
  // 状态进主界面。
  if (!store.health) return "unreachable";
  if (!store.health.local_auth_enabled) return "ok";
  try {
    await api.settings();
    return "ok";
  } catch (error) {
    if (error instanceof ApiError && error.isUnauthorized) return "unauthorized";
    // 连不上时记下原因，让验证页能说明"是服务没起来"而不是"Key 错了"。
    store.connectionError = errorText(error);
    return "unreachable";
  }
}

// 未通过鉴权（含"连不上、无从判断"）时整页只渲染验证页：不渲染应用栏与导航，页面
// 模块也不会带着错误 Key 加载并把 401 缓存成业务错误。authorized 只在确认可访问后
// 置真，所以服务掉线时也停在验证页，而不是拿没验证过的状态进主界面。
function requiresKey() {
  return !store.authorized;
}

async function loadMetrics(force = false) {
  if (requiresKey()) return;
  try {
    const data = await api.metrics(1);
    if (data && data.total) { store.metrics = data; store.metricsError = null; }
    else if (!store.metrics) { store.metrics = data; store.metricsError = null; }
  } catch (error) {
    if (error instanceof ApiError && error.isUnauthorized) { store.authorized = false; }
    store.metricsError = errorText(error);
  }
  if (force || store.page === "overview" || store.page === "activity") renderContent();
}

function scheduleTimers() {
  clearInterval(healthTimer);
  clearInterval(metricsTimer);
  healthTimer = setInterval(loadHealth, 5000);
  const interval = store.page === "activity" ? 2000 : 15000;
  metricsTimer = setInterval(() => loadMetrics(), interval);
  // 活动页需要更快的刷新节奏，切页后立即生效。
  setTimeout(() => { if (store.page === "activity") scheduleTimers(); }, 0);
}

function serviceTone() {
  if (store.connectionError) return "bad";
  if (!store.health) return "warn";
  if (store.health.status !== "ok") return "warn";
  return "good";
}

function serviceLabel() {
  if (store.connectionError) return "服务未连接";
  if (!store.health) return "正在连接";
  if (store.health.status !== "ok") return "服务未运行";
  if (requiresKey()) return "需要本地鉴权 Key";
  return "服务运行中";
}

export function renderShell() {
  // 未通过鉴权时整页只有验证页：应用栏与导航都不渲染，深链、切页、会话中途失效
  // 都会回到这里，不给"带着无效 Key 进入主界面"留任何入口。
  if (requiresKey()) {
    contentHost = null;
    mount(appHost, loginPage());
    return;
  }

  const nav = h("nav.nav", { "aria-label": "主导航" },
    PAGES.map((group) => h("div", {},
      h("div.group-label", group.group),
      group.items.map((item) => h("button.nav-item", {
        type: "button",
        "aria-current": store.page === item.id ? "page" : null,
        onClick: () => navigate(item.id),
      }, h("span.icon", item.icon), item.label)),
    )),
  );

  const bar = h("header.app-bar", {},
    h("div.brand", "AMKR WebUI", h("small", "Auto Model Key Router")),
    h("div.spacer"),
    h("span.status-dot", { class: `tone-${serviceTone()}`, title: serviceLabel() }),
    h("span.bar-meta", serviceLabel()),
    store.health?.version ? h("span.bar-meta", `v${store.health.version}`) : null,
  );

  contentHost = h("main.content", { id: "content" });
  mount(appHost, bar, h("div.shell", nav, contentHost));
  renderContent();
}

// 版本门槛：管理 API 是 4.0.0 起稳定对外，低于此版本只开放只读诊断页。
const MIN_VERSION = "4.0.0";

export function versionCompatible(version) {
  if (!version) return true;
  const parse = (value) => value.split(".").slice(0, 3).map((part) => parseInt(part, 10));
  const current = parse(version);
  const minimum = parse(MIN_VERSION);
  if (current.length !== 3 || current.some(Number.isNaN)) return false;
  for (let i = 0; i < 3; i += 1) {
    if (current[i] !== minimum[i]) return current[i] > minimum[i];
  }
  return true;
}

export function requiresCompatible(page, children) {
  if (versionCompatible(store.health?.version)) return children;
  return h("div.stack", {},
    notice(`当前 AMKR ${store.health?.version || "未知版本"}，WebUI 至少需要 ${MIN_VERSION}。升级前仅开放设置与只读诊断。`, "warn"),
    empty("后端版本不兼容。"),
  );
}

function renderContent() {
  if (!contentHost) return;
  const page = currentPage();
  try {
    const node = page.render(ctx());
    mount(contentHost, node);
  } catch (error) {
    mount(contentHost, notice(`页面渲染失败: ${errorText(error)}`, "error"));
  }
}

// ══ 独立验证页 ══
// 未通过鉴权时整页只有它：不渲染应用栏、导航与任何页面，深链、切页、会话中途失效
// 都回到这里，不给"带着无效 Key 进入主界面"留入口。
// 节点全程复用：健康轮询每 5s 会重绘整壳，重建会让用户已粘贴一半的 Key 与焦点消失。
const KEY_HINT = "可在终端执行 amkr --show-api-key 获取本地鉴权 Key。";
const KEY_INVALID = "本地鉴权 Key 无效，请重新核对后重试。可在终端执行 amkr --show-api-key 获取。";

let loginNode = null;
let loginNotice = null;   // askLogin 传入的会话失效原因
let loginPaint = null;    // 就地重绘状态提示，不重建输入框

function loginPage() {
  if (loginNode) { loginPaint(); return loginNode; }

  const keyInput = input({ type: "password", placeholder: "粘贴本地鉴权 Key", autocomplete: "off" });
  const statusHost = h("div");
  const retryHost = h("div.btn-row");
  const footHost = h("div.login-foot");

  // 成功路径：整页重载。各页面模块的 state 是模块级缓存，重载是确定干净且能顺带
  // 刷新 config_revision 的收尾（否则保存会 409）。
  const succeed = () => {
    store.authorized = true;
    store.connectionError = null;
    loginNotice = null;
    loginNode = null;
    location.reload();
  };

  const connectBtn = buttonNode("连接", { onClick: () => submit() });

  async function submit() {
    const value = keyInput.value.trim();
    if (!value) {
      loginNotice = { text: "请先填写本地鉴权 Key。", tone: "warn" };
      loginPaint();
      return;
    }
    // 清掉上一次的提示，否则"先填 Key"之类的旧提示会盖住本次的真实结果。
    loginNotice = null;
    connectBtn.disabled = true;
    setKey(value);
    const status = await verifyAccess();
    connectBtn.disabled = false;
    if (status === "ok") { succeed(); return; }
    // 连不上时保留 Key：可能只是服务还没起来，重试即可，不该让用户重贴一次。
    if (status === "unauthorized") { setKey(""); loginNotice = { text: KEY_INVALID, tone: "error" }; }
    loginPaint();
  }

  keyInput.addEventListener("keydown", (event) => { if (event.key === "Enter") submit(); });

  loginPaint = () => {
    const failure = store.connectionError;
    render(statusHost,
      loginNotice
        ? notice(loginNotice.text, loginNotice.tone)
        : failure
          ? notice(`无法连接 AMKR 服务：${failure}`, "error")
          : notice(KEY_HINT, "info"),
    );
    // 已经拿不到服务时才给重试入口；Key 本身错了要改 Key，重试没有意义。
    render(retryHost, failure && !loginNotice
      ? buttonNode("重试连接", {
          variant: "secondary",
          onClick: async () => {
            store.connectionError = null;
            const status = await verifyAccess();
            if (status === "ok") { succeed(); return; }
            // 服务恢复后才发现 Key 也失效了，同样要清掉并说明原因。
            if (status === "unauthorized") { setKey(""); loginNotice = { text: KEY_INVALID, tone: "error" }; }
            loginPaint();
          },
        })
      : null);
    render(footHost, loginFoot());
  };

  loginNode = h("div.login-shell", {},
    h("div.card.login-card", {},
      h("div.login-head", {},
        h("span.login-mark", {}, "AMKR"),
        h("div", {}, h("h2", "连接到 AMKR"), h("p.muted", "管理接口需要本地鉴权 Key")),
      ),
      h("div.stack", {}, statusHost, field("本地鉴权 Key", keyInput), retryHost),
      h("div.btn-row", { style: { marginTop: "16px" } }, connectBtn),
      footHost,
    ),
  );
  loginPaint();
  return loginNode;
}

// 独立页没有导航栏，把版本与入口放在页脚：能一眼确认连的是哪个实例。
function loginFoot() {
  const health = store.health;
  return [
    health?.version ? `版本 v${health.version}` : null,
    health?.base_url || null,
  ].filter(Boolean).join(" · ") || "AMKR WebUI";
}

function askLogin(message) {
  loginNotice = message ? { text: message, tone: "warn" } : null;
  loginNode = null;
  store.authorized = false;
  renderShell();
}

async function boot() {
  root = document.getElementById("root");
  // 应用内容与 Toast 分层：整壳重绘只替换 appHost，不能顺手把 Toast 容器清掉。
  appHost = h("div.app");
  root.append(appHost);
  installToastHost(root);
  store.page = pageFromHash();

  // 先挂 hash 监听：未授权时 renderShell 一律落回验证页，因此浏览器前进/后退或
  // 手改地址栏都无法绕过鉴权。
  window.addEventListener("hashchange", () => {
    const next = pageFromHash();
    if (next !== store.page) { store.page = next; renderShell(); scheduleTimers(); }
  });

  // 已在验证页上时 401 由提交按钮自己提示，这里只管会话中途失效的情况。
  onUnauthorized(() => { if (store.authorized) askLogin("本地鉴权 Key 已失效，请重新输入。"); });

  await fetchHealth();
  // 一律先验证再进主界面：需要鉴权时要校验 Key，连不上时也无从判断是否放行。
  const status = await verifyAccess();
  if (status !== "ok") {
    if (status === "unauthorized") setKey("");
    store.authorized = false;
    // 保留地址栏里的深链：验证通过并重载后回到用户原本要去的页面。
    renderShell();
    scheduleTimers();
    return;
  }
  store.authorized = true;
  renderShell();
  await loadMetrics(true);
  scheduleTimers();
}

export { boot };
