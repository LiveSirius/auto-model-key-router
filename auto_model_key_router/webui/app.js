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

let root = null;
let contentHost = null;
let navHost = null;
let healthTimer = null;
let metricsTimer = null;
let pending = false;

export function currentPage() {
  for (const group of PAGES) {
    const found = group.items.find((item) => item.id === store.page);
    if (found) return found;
  }
  return PAGES[0].items[0];
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
  renderShell();
}

// localStorage 里有 Key 不代表 Key 可用（可能已被重置或来自旧版本），必须实际请求一次。
// 返回值区分"未授权"与"连不上"，前者才清掉本地 Key，避免网络抖动误删有效 Key。
async function verifyAccess() {
  if (!store.health?.local_auth_enabled) return "ok";
  try {
    await api.settings();
    return "ok";
  } catch (error) {
    return error instanceof ApiError && error.isUnauthorized ? "unauthorized" : "unreachable";
  }
}

// 未授权时整壳只渲染登录卡，页面模块不会带着错误 Key 加载并缓存 401，授权后自然是干净状态。
function requiresKey() {
  return Boolean(store.health?.local_auth_enabled) && !store.authorized;
}

async function loadMetrics(force = false) {
  if (!store.authorized && store.health?.local_auth_enabled) return;
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
  if (store.health.local_auth_enabled && !store.authorized) return "需要本地鉴权 Key";
  return "服务运行中";
}

export function renderShell() {
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
  mount(root, bar, h("div.shell", nav, contentHost));
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
  if (requiresKey()) {
    mount(contentHost, loginCard());
    return;
  }
  const page = currentPage();
  try {
    const node = page.render(ctx());
    mount(contentHost, node);
  } catch (error) {
    mount(contentHost, notice(`页面渲染失败: ${errorText(error)}`, "error"));
  }
}

// 复用同一个节点：健康轮询会整壳重绘，新建节点会让用户已粘贴一半的 Key 消失。
let loginNode = null;
let loginError = null;

function loginCard() {
  if (loginNode) return loginNode;
  const keyInput = input({ type: "password", placeholder: "粘贴本地鉴权 Key", autocomplete: "off" });
  const errorHost = h("div");
  const submit = async () => {
    const value = keyInput.value.trim();
    if (!value) {
      render(errorHost, notice("请先填写本地鉴权 Key。", "warn"));
      return;
    }
    setKey(value);
    const status = await verifyAccess();
    if (status !== "ok") {
      setKey("");
      store.authorized = false;
      render(errorHost, notice(
        status === "unauthorized"
          ? "本地鉴权 Key 无效，请重新核对后重试。可在终端执行 amkr --show-api-key 获取。"
          : "无法连接 AMKR 服务，请确认服务正在运行。",
        "error",
      ));
      return;
    }
    store.authorized = true;
    store.connectionError = null;
    loginNode = null;
    // ponytail: 各页面模块的 state 是模块级缓存，重新鉴权后可能残留旧数据与旧
    // config_revision（保存会 409）。整页重载是最省事且确定干净的收尾。
    location.reload();
  };
  keyInput.addEventListener("keydown", (event) => { if (event.key === "Enter") submit(); });
  loginNode = h("div.card", {}, h("div.card-head", h("h3", "连接到 AMKR")),
    h("div.stack", {},
      notice(loginError || "管理接口需要本地鉴权 Key。可在终端执行 amkr --show-api-key 获取。", "info"),
      field("本地鉴权 Key", keyInput),
      errorHost,
    ),
    h("div.btn-row", { style: { marginTop: "16px" } },
      buttonNode("连接", { onClick: submit }),
    ));
  return loginNode;
}

function askLogin(message) {
  loginError = message || null;
  if (loginNode) loginNode = null;
  store.authorized = false;
  renderShell();
}
async function boot() {
  root = document.getElementById("root");
  installToastHost(root);
  store.page = currentPage().id;

  // 已在登录卡上时 401 由 submit 自己处理错误提示，这里只管会话中途失效的情况。
  onUnauthorized(() => { if (store.authorized) askLogin("本地鉴权 Key 已失效，请重新输入。"); });

  await fetchHealth();
  if (store.health?.local_auth_enabled) {
    // 有 Key 也要先验证：失效的 Key 会让每个页面都缓存 401 报错，而不是回到登录卡。
    const status = await verifyAccess();
    if (status !== "ok") {
      if (status === "unauthorized") setKey("");
      store.authorized = false;
      loginError = status === "unreachable" ? "无法连接 AMKR 服务，请确认服务正在运行。" : null;
      renderShell();
      scheduleTimers();
      return;
    }
  }
  store.authorized = true;
  renderShell();
  await loadMetrics(true);
  scheduleTimers();

  window.addEventListener("hashchange", () => {
    const next = location.hash.replace(/^#\/?/, "") || "overview";
    if (next !== store.page) { store.page = next; renderShell(); scheduleTimers(); }
  });
}

export { boot };
