// 模型路由：路由别名、模式与目标 Key 顺序。

import { h, errorText } from "../dom.js";
import { api } from "../api.js";
import { card, cardHead, notice, badge, empty, loading, render, toast, buttonNode, input, select, confirmDialog } from "../ui.js";

const MODES = [
  { value: "", label: "默认策略" },
  { value: "round_robin", label: "轮询" },
  { value: "priority", label: "优先级" },
  { value: "only_first", label: "首 Key" },
];

const modeLabel = (value) => (MODES.find((mode) => mode.value === (value || "")) || MODES[0]).label;

const state = {
  routes: [],
  providers: [],
  revision: null,
  loading: true,
  error: null,
  active: "",
  editing: null,
  saving: false,
};

let host = null;

async function load() {
  const [routes, providers] = await Promise.all([api.routes(), api.providers()]);
  state.routes = routes.routes || [];
  state.revision = routes.config_revision;
  state.providers = providers.providers || [];
  if (!state.routes.some((route) => route.id === state.active)) {
    state.active = state.routes[0]?.id || "";
  }
}

// 候选目标：探测到该模型的 Key，且尚未绑定。
function candidates(route) {
  const existing = new Set((route.targets || []).map((target) => `${target.provider}|\${target.key}`));
  const list = [];
  for (const provider of state.providers) {
    for (const key of provider.keys || []) {
      const models = key.capabilities?.models || [];
      if (!models.includes(route.id)) continue;
      if (existing.has(`${provider.id}|${key.name}`)) continue;
      list.push({ provider: provider.id, key: key.name });
    }
  }
  return list;
}

function routeEditor(route) {
  const aliasInput = input({ value: (route.aliases || []).join(", "), placeholder: "逗号分隔，留空表示无别名" });
  const hiddenInput = input({ value: (route.hidden_aliases || []).join(", "), placeholder: "可调用但不列出，逗号分隔" });
  const modeSelect = select(MODES, { value: route.routing_mode || "" });
  const errorHost = h("div");
  const targets = [...(route.targets || [])];

  const listHost = h("div.stack.tight");
  const drawTargets = () => {
    render(listHost,
      targets.length
        ? targets.map((target, index) => h("div.inline", { style: { padding: "8px 12px", background: "#fafafa", borderRadius: "4px" } },
            h("span.mono", `${target.provider} / ${target.key} / ${target.upstream_model}`),
            h("span", { style: { flex: "1" } }),
            buttonNode("上移", { small: true, variant: "text", disabled: index === 0, onClick: () => { [targets[index - 1], targets[index]] = [targets[index], targets[index - 1]]; drawTargets(); } }),
            buttonNode("下移", { small: true, variant: "text", disabled: index === targets.length - 1, onClick: () => { [targets[index + 1], targets[index]] = [targets[index], targets[index + 1]]; drawTargets(); } }),
            buttonNode("移除", { small: true, variant: "text", onClick: () => { targets.splice(index, 1); drawTargets(); } }),
          ))
        : h("p.muted", "尚未绑定目标 Key。"),
    );
  };
  drawTargets();

  const options = candidates(route);
  const candidateHost = options.length
    ? h("div.btn-row", {}, options.map((candidate) => buttonNode(`+ ${candidate.provider} / ${candidate.key}`, {
        small: true, variant: "secondary",
        onClick: () => {
          targets.push({ provider: candidate.provider, key: candidate.key, upstream_model: route.id });
          drawTargets();
          render(candidateHost);
        },
      })))
    : h("p.muted", `没有其它探测到模型 ${route.id} 的 Key。`);

  return h("div.stack", {},
    h("div.form-grid", {},
      h("label.field", h("span", "编辑别名"), aliasInput),
      h("label.field", h("span", "编辑隐藏别名"), hiddenInput),
      h("label.field", h("span", "编辑模式"), modeSelect),
    ),
    h("p.muted", "隐藏别名可以直接调用，但不会出现在 /v1/models 里；各目标 Key 的上游模型名也会自动获得同样待遇。"),
    h("div", {}, h("h4", "绑定目标 Key"), h("p.muted", `仅显示探测到模型 ${route.id} 的 Key。`), candidateHost),
    listHost,
    errorHost,
    h("div.btn-row", {},
      buttonNode(state.saving ? "保存中…" : "保存路由", {
        disabled: state.saving,
        onClick: async () => {
          state.saving = true;
          draw();
          try {
            const aliases = aliasInput.value.split(",").map((item) => item.trim()).filter(Boolean);
            const hiddenAliases = hiddenInput.value.split(",").map((item) => item.trim()).filter(Boolean);
            await api.updateRoute(state.revision, route.id, targets, aliases, hiddenAliases, modeSelect.value || null);
            await load();
            state.editing = null;
            toast("路由已保存。");
          } catch (error) {
            render(errorHost, notice(`保存失败: ${errorText(error)}`, "error"));
            if (error.status === 409) load();
          }
          state.saving = false;
          draw();
        },
      }),
      buttonNode("取消", { variant: "text", onClick: () => { state.editing = null; draw(); } }),
      buttonNode("删除路由", {
        variant: "danger",
        disabled: state.saving,
        onClick: () => confirmDialog({
          title: "删除路由",
          message: `删除模型路由 ${route.id}？绑定该模型的 Key 会一并解除。`,
          confirmLabel: "删除",
          danger: true,
          onConfirm: async () => {
            try {
              await api.deleteRoute(state.revision, route.id);
              await load();
              state.editing = null;
              toast("路由已删除。");
              draw();
            } catch (error) { toast(errorText(error), "error"); }
          },
        }),
      }),
    ),
  );
}

export function renderRouting(context) {
  host = h("div.stack");
  if (state.loading) {
    render(host, h("div.page-head", h("h1", "模型路由")), loading("正在读取模型路由。"));
    (async () => {
      try { await load(); state.error = null; } catch (error) { state.error = errorText(error); }
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
  const children = [
    h("div.page-head", {},
      h("div", {}, h("h1", "模型路由"),
        h("p.sub", "管理路由别名和路由模式；模型的目标 Key 在下方按顺序排列。")),
      h("div.spacer"),
      state.revision ? badge(`版本 ${String(state.revision).slice(0, 12)}`, "muted") : null,
    ),
  ];
  if (state.error) children.push(notice(`无法读取或写入模型路由: ${state.error}`, "error"));
  if (!state.routes.length) {
    children.push(empty("尚未配置模型路由。请先在供应商页添加 Key 并绑定其服务模型，路由会自动出现在这里。"));
    render(host, children);
    return;
  }

  children.push(h("div.tabs", { role: "tablist" }, state.routes.map((route) => h("button.tab", {
    type: "button", role: "tab", "aria-selected": String(route.id === state.active),
    onClick: () => { state.active = route.id; state.editing = null; draw(); },
  }, `模型 · ${route.id}`))));

  const route = state.routes.find((item) => item.id === state.active);
  if (!route) { render(host, children); return; }

  if (state.editing === route.id) {
    children.push(card(routeEditor(route)));
  } else {
    children.push(card(
      cardHead(route.id,
        badge(modeLabel(route.routing_mode), "muted"),
        badge(`${(route.targets || []).length} 个目标`, "muted"),
        buttonNode("编辑", { small: true, variant: "text", onClick: () => { state.editing = route.id; draw(); } }),
      ),
      h("div.stack.tight", {},
        h("div", {}, h("span.muted", "别名："), (route.aliases || []).length ? h("span.mono", route.aliases.join(", ")) : "无别名"),
        h("div", {}, h("span.muted", "隐藏别名："), (route.hidden_aliases || []).length ? h("span.mono", route.hidden_aliases.join(", ")) : "无隐藏别名"),
        h("div", {}, h("span.muted", "路由目标（按顺序）：")),
        (route.targets || []).length
          ? h("ul", { "aria-label": `${route.id} 的路由目标`, style: { margin: "0", paddingLeft: "20px" } },
              route.targets.map((target) => h("li.mono", `${target.provider} / ${target.key} / ${target.upstream_model}`)))
          : h("p.muted", "尚未绑定目标 Key。"),
      ),
    ));
  }
  render(host, children);
}
