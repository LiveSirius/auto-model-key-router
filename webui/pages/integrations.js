// 集成：把 Claude Code / Codex / Pi Agent 接到本机 AMKR。

import { h, errorText } from "../dom.js";
import { api } from "../api.js";
import { card, cardHead, notice, badge, loading, render, toast, buttonNode, select, confirmDialog } from "../ui.js";

const AGENTS = [
  { id: "claude-code", name: "Claude Code" },
  { id: "codex", name: "Codex" },
  { id: "pi-agent", name: "Pi Agent" },
];

const MODES = [
  { value: "unified-model", label: "统一模型" },
  { value: "native", label: "原生模型" },
];

// 变更字段预览：与各 Agent 的目标文件结构一一对应。
const PREVIEW = {
  "claude-code": {
    "unified-model": ["env.ANTHROPIC_BASE_URL", "env.ANTHROPIC_AUTH_TOKEN", "env.ANTHROPIC_MODEL"],
    native: ["env.ANTHROPIC_BASE_URL", "env.ANTHROPIC_AUTH_TOKEN"],
  },
  codex: {
    "unified-model": ["model_provider", "model", "model_providers.OpenAI", "auth.json"],
    native: ["model_provider", "model_providers.OpenAI", "auth.json"],
  },
  "pi-agent": {
    "unified-model": ["providers.amkr", "providers.amkr.baseUrl", "providers.amkr.apiKey", "providers.amkr.models"],
  },
};

const state = { items: [], loading: true, error: null, busy: null, modes: {}, errors: {} };

let host = null;
let xtxRef = null;

function itemFor(agent) {
  return state.items.find((item) => item.agent === agent) || null;
}

function statusBadge(item) {
  if (!item) return badge("正在读取", "muted");
  if (item.error) return badge("操作失败", "bad");
  if (item.current_is_applied) return badge(`已接管 · ${item.mode || "未知模式"}`, "good");
  if (item.backup_available) return badge("配置已变更 · 可回退", "warn");
  if (item.target_exists) return badge("检测到配置", "muted");
  return badge("未找到配置", "muted");
}

function modeOf(agent) {
  if (state.modes[agent]) return state.modes[agent];
  const item = itemFor(agent);
  return item && MODES.some((mode) => mode.value === item.mode) ? item.mode : "unified-model";
}

async function apply(agent) {
  state.busy = agent;
  state.errors[agent] = null;
  draw();
  try {
    await api.applyIntegration(agent, modeOf(agent));
    await reload();
    toast(`${itemFor(agent)?.display_name || agent} 已接管配置。`);
  } catch (error) {
    state.errors[agent] = errorText(error);
  }
  state.busy = null;
  draw();
}

async function reload() {
  const data = await api.integrations();
  state.items = data.integrations || [];
}

export function renderIntegrations(context) {
  xtxRef = context;
  host = h("div.stack");
  if (state.loading) {
    render(host, h("div.page-head", h("h1", "集成")), loading("正在读取集成状态。"));
    (async () => {
      try { await reload(); state.error = null; } catch (error) { state.error = errorText(error); }
      state.loading = false;
      draw();
    })();
    return host;
  }
  draw();
  return host;
}

function draw() {
  if (!host) return;
  const health = xtxRef?.store?.health;
  const baseUrl = health?.base_url;
  const authEnabled = health?.local_auth_enabled;
  const children = [
    h("div.page-head", {},
      h("div", {}, h("h1", "集成"), h("p.sub", "让本机的 CLI 客户端通过 AMKR 统一入口发请求。")),
      h("div.spacer"),
      buttonNode("刷新状态", { small: true, variant: "secondary", onClick: async () => { try { await reload(); draw(); } catch (error) { state.error = errorText(error); draw(); } } }),
    ),
  ];
  if (state.error) children.push(notice(`读取集成状态失败: ${state.error}`, "error"));
  if (!baseUrl) children.push(notice("尚未连接到 AMKR 服务，无法应用集成。", "warn"));
  else if (!authEnabled) children.push(notice("本地鉴权未启用：集成需要本地鉴权 Key 才能写入客户端配置。", "warn"));

  for (const agent of AGENTS) {
    const item = itemFor(agent.id);
    const fixedMode = agent.id === "pi-agent";
    const busy = state.busy === agent.id;
    children.push(card(
      cardHead(item?.display_name || agent.name,
        statusBadge(item),
        busy ? h("span.spinner") : null,
      ),
      h("p.muted", item?.target_path ? `目标文件 ${item.target_path}` : "正在读取配置状态。"),
      state.errors[agent.id] ? notice(state.errors[agent.id], "error") : null,
      item?.error ? notice(item.error, "error") : null,
      fixedMode
        ? h("div.inline", {}, h("span.muted", "路由模式"), badge("统一模型", "muted"))
        : h("label.field", { style: { maxWidth: "240px" } }, h("span", "路由模式"),
            select(MODES, {
              value: modeOf(agent.id),
              disabled: busy,
              onChange: (event) => { state.modes[agent.id] = event.target.value; draw(); },
            })),
      h("details", {}, h("summary.muted", `变更字段 · ${(PREVIEW[agent.id]?.[modeOf(agent.id)] || []).length} 项`),
        h("div.stack.tight", { style: { marginTop: "8px" } },
          (PREVIEW[agent.id]?.[modeOf(agent.id)] || []).map((name) => h("div.mono", name))),
      ),
      h("div.btn-row", { style: { marginTop: "16px" } },
        buttonNode(busy ? "正在处理" : "应用", {
          disabled: !!state.busy || !baseUrl,
          onClick: () => apply(agent.id),
        }),
        buttonNode("回退", {
          variant: "secondary",
          disabled: !!state.busy || !item?.backup_available,
          onClick: () => confirmDialog({
            title: "回退集成",
            message: `回退 ${item?.display_name || agent.name} 的原配置？`,
            confirmLabel: "回退",
            danger: true,
            onConfirm: async () => {
              state.busy = agent.id;
              draw();
              try {
                await api.rollbackIntegration(agent.id);
                await reload();
                toast("已回退到原配置。");
              } catch (error) {
                state.errors[agent.id] = errorText(error);
              }
              state.busy = null;
              draw();
            },
          }),
        }),
      ),
    ));
  }

  if (baseUrl) {
    children.push(h("p.muted", `目标地址 ${baseUrl}。${authEnabled ? "本地鉴权已启用。" : "本地鉴权未启用。"}`));
  }
  render(host, children);
}
