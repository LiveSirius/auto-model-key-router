// WebUI 外壳：连接状态、鉴权、导航与全局轮询。
// 轮询节奏对齐 Keyloom：健康 5s、指标 15s（活动页 2s）。

import { h, mount, errorText } from "./dom.js";
import { api, getKey, setKey, ApiError } from "./api.js";
import { installToastHost, notice, buttonNode, input, field, empty } from "./ui.js";

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

async function loadHealth() {
  try {
    store.health = await api.health();
    store.connectionError = null;
  } catch (error) {
    store.health = null;
    store.connectionError = errorText(error);
  }
  renderShell();
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
  const page = currentPage();
  try {
    const node = page.render(ctx());
    mount(contentHost, node);
  } catch (error) {
    mount(contentHost, notice(`页面渲染失败: ${errorText(error)}`, "error"));
  }
}

function askLogin(message) {
  const keyInput = input({ type: "password", placeholder: "粘贴本地鉴权 Key", autocomplete: "off" });
  const node = h("div.stack", {},
    notice(message || "管理接口需要本地鉴权 Key。可在终端执行 amkr --show-api-key 获取。", "info"),
    field("本地鉴权 Key", keyInput),
  );
  const submit = async () => {
    setKey(keyInput.value.trim());
    store.authorized = true;
    store.connectionError = null;
    await loadHealth();
    await loadMetrics(true);
    renderShell();
  };
  keyInput.addEventListener("keydown", (event) => { if (event.key === "Enter") submit(); });
  mount(contentHost, h("div.card", {}, h("div.card-head", h("h3", "连接到 AMKR")), node,
    h("div.btn-row", { style: { marginTop: "16px" } },
      buttonNode("连接", { onClick: submit }),
    )));
}

async function boot() {
  root = document.getElementById("root");
  installToastHost(root);
  store.page = currentPage().id;

  await loadHealth();
  if (store.health?.local_auth_enabled && !getKey()) {
    store.authorized = false;
    renderShell();
    askLogin();
  } else {
    store.authorized = true;
    await loadMetrics(true);
    renderShell();
  }
  scheduleTimers();

  window.addEventListener("hashchange", () => {
    const next = location.hash.replace(/^#\/?/, "") || "overview";
    if (next !== store.page) { store.page = next; renderShell(); scheduleTimers(); }
  });
}

export { boot };
