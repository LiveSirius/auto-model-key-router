// 统一模型：主/回退/图像映射与推理强度。

import { h, mount, errorText } from "../dom.js";
import { api } from "../api.js";
import { card, cardHead, notice, badge, empty, loading, render, toast, buttonNode, select, confirmDialog, kv } from "../ui.js";

const EFFORTS = [
  { value: "", label: "默认" },
  { value: "none", label: "none" },
  { value: "minimal", label: "minimal" },
  { value: "low", label: "low" },
  { value: "medium", label: "medium" },
  { value: "high", label: "high" },
  { value: "xhigh", label: "xhigh" },
  { value: "max", label: "max" },
];

const state = { unified: null, models: [], revision: null, loading: true, error: null, editing: false, saving: false, notice: null };

let host = null;

const enabledKeys = (modelId) =>
  (state.models.find((model) => model.id === modelId)?.keys || []).filter((key) => key.enabled).map((key) => key.name);

async function load() {
  const [unified, models] = await Promise.all([api.unified(), api.models()]);
  state.unified = unified.unified_model || null;
  state.revision = unified.config_revision ?? models.config_revision;
  state.models = models.models || [];
}

function statusText(unified) {
  const primary = unified?.default?.primary;
  if (!primary) return "未启用";
  return primary.key ? `固定 Key · ${primary.key}` : "自动路由";
}

function editor() {
  const primaryModel = state.unified?.default?.primary?.model || state.models[0]?.id || "";
  let primaryKey = state.unified?.default?.primary?.key || "";
  let routing = primaryKey ? "key" : "auto";
  let fallbackModel = state.unified?.default?.fallback?.model || "";
  let fallbackKey = state.unified?.default?.fallback?.key || "";
  let imageModel = state.unified?.image?.primary?.model || "";
  let imageKey = state.unified?.image?.primary?.key || "";
  let embeddingModel = state.unified?.embeddings?.primary?.model || "";
  let embeddingKey = state.unified?.embeddings?.primary?.key || "";
  let effort = state.models.find((model) => model.id === primaryModel)?.reasoning_effort || "";

  const errorHost = h("div");
  const formHost = h("div.stack");

  const modelOptions = () => state.models.map((model) => ({ value: model.id, label: model.id }));
  const keyOptions = (modelId, includeAuto) => {
    const options = includeAuto ? [{ value: "", label: "自动路由" }] : [];
    return options.concat(enabledKeys(modelId).map((name) => ({ value: name, label: name })));
  };

  const drawForm = () => {
    const primaryKeys = enabledKeys(primaryModel);
    if (routing === "key" && primaryKey && !primaryKeys.includes(primaryKey)) {
      primaryKey = primaryKeys[0] || "";
      if (!primaryKey) routing = "auto";
    }
    const model = state.models.find((item) => item.id === primaryModel);

    const modelSelect = select(modelOptions(), {
      value: primaryModel,
      disabled: state.saving,
      onChange: (event) => {
        const next = event.target.value;
        // 选中的模型正好是当前回退时，两者互换。
        if (fallbackModel && next === fallbackModel) {
          fallbackModel = primaryModel;
          fallbackKey = routing === "key" ? primaryKey : "";
        }
        state.unified = state.unified || { default: { primary: {} }, image: null };
        state.unified.default.primary = { model: next, key: routing === "key" ? primaryKey : null };
        effort = state.models.find((item) => item.id === next)?.reasoning_effort || "";
        const keys = enabledKeys(next);
        primaryKey = keys.includes(primaryKey) ? primaryKey : keys[0] || "";
        if (routing === "key" && !keys.length) routing = "auto";
        render(host, editor());
      },
    });

    const routingRadios = h("div.inline", {},
      ["auto", "key"].map((value) => h("label.check", {},
        h("input", {
          type: "radio", name: "routing", value, checked: routing === value, disabled: state.saving || (value === "key" && !primaryKeys.length),
          onChange: () => { routing = value; state.unified = state.unified || { default: { primary: {} }, image: null }; state.unified.default.primary = { model: primaryModel, key: value === "key" ? primaryKey || primaryKeys[0] || null : null }; primaryKey = value === "key" ? primaryKey || primaryKeys[0] || "" : primaryKey; render(host, editor()); },
        }),
        value === "auto" ? "自动路由" : "固定 Key",
      )),
    );

    mount(formHost,
      h("div.form-grid", {},
        h("label.field", h("span", "模型"), modelSelect),
        h("label.field", h("span", "推理强度"), select(EFFORTS, {
          value: effort, disabled: state.saving,
          onChange: (event) => { effort = event.target.value; },
        })),
      ),
      h("div.field", h("span", "路由方式"), routingRadios),
      routing === "key"
        ? h("label.field", h("span", "Key"), select(keyOptions(primaryModel, false), {
            value: primaryKey, disabled: state.saving,
            onChange: (event) => { primaryKey = event.target.value; state.unified.default.primary = { model: primaryModel, key: primaryKey }; },
          }))
        : null,
      h("div", { style: { marginTop: "8px" } }, kv([
        ["路由策略", model?.routing_mode || "round_robin"],
        ["推理强度", effort || "默认"],
        ["启用 Key", String(primaryKeys.length)],
        ["别名", (model?.aliases || []).join(", ") || "无"],
      ])),
      h("hr.divider"),
      h("div.form-grid", {},
        h("label.field", h("span", "回退模型"), select([{ value: "", label: "不启用回退" }].concat(modelOptions().filter((option) => option.value !== primaryModel)), {
          value: fallbackModel, disabled: state.saving,
          onChange: (event) => { fallbackModel = event.target.value; fallbackKey = ""; drawForm(); },
        })),
        h("label.field", h("span", "回退 Key"), select(keyOptions(fallbackModel, true), {
          value: fallbackKey, disabled: state.saving || !fallbackModel,
          onChange: (event) => { fallbackKey = event.target.value; },
        })),
      ),
      h("div.form-grid", {},
        h("label.field", h("span", "图像模型"), select([{ value: "", label: "不配置映射" }].concat(modelOptions()), {
          value: imageModel, disabled: state.saving,
          onChange: (event) => { imageModel = event.target.value; imageKey = ""; drawForm(); },
        })),
        h("label.field", h("span", "图像 Key"), select(keyOptions(imageModel, true), {
          value: imageKey, disabled: state.saving || !imageModel,
          onChange: (event) => { imageKey = event.target.value; },
        })),
      ),
      h("div.form-grid", {},
        h("label.field", h("span", "嵌入模型"), select([{ value: "", label: "不配置映射" }].concat(modelOptions()), {
          value: embeddingModel, disabled: state.saving,
          onChange: (event) => { embeddingModel = event.target.value; embeddingKey = ""; drawForm(); },
        })),
        h("label.field", h("span", "嵌入 Key"), select(keyOptions(embeddingModel, true), {
          value: embeddingKey, disabled: state.saving || !embeddingModel,
          onChange: (event) => { embeddingKey = event.target.value; },
        })),
      ),
    );

    const validate = () => {
      if (!primaryModel) return "请先选择模型。";
      if (routing === "key" && !primaryKey) return "当前模型没有可用的启用 Key。";
      if (fallbackModel && fallbackModel === primaryModel) return "回退模型不能与主模型相同。";
      return null;
    };

    const actions = h("div.btn-row", {},
      buttonNode(state.saving ? "正在保存" : "保存", {
        disabled: state.saving,
        onClick: async () => {
          const problem = validate();
          if (problem) { render(errorHost, notice(problem, "error")); return; }
          state.saving = true;
          render(host, editor());
          try {
            let revision = state.revision;
            const stored = state.models.find((item) => item.id === primaryModel)?.reasoning_effort || "";
            if ((effort || "") !== (stored || "")) {
              const updated = await api.updateModelEffort(revision, primaryModel, effort || null);
              revision = updated.config_revision || revision;
            }
            const payload = {
              default: {
                primary: { model: primaryModel, key: routing === "key" ? primaryKey : null },
                fallback: fallbackModel ? { model: fallbackModel, key: fallbackKey || null } : null,
              },
              image: imageModel
                ? { primary: { model: imageModel, key: imageKey || null } }
                : null,
              embeddings: embeddingModel
                ? { primary: { model: embeddingModel, key: embeddingKey || null } }
                : null,
            };
            await api.updateUnified(revision, payload);
            await load();
            state.editing = false;
            toast("统一模型已更新。");
            draw();
          } catch (error) {
            render(errorHost, notice(`统一模型操作失败: ${errorText(error)}`, "error"));
            state.saving = false;
            render(host, editor());
          }
        },
      }),
      buttonNode("取消", { variant: "text", disabled: state.saving, onClick: () => { state.editing = false; draw(); } }),
    );

    formHost.append(actions, errorHost);
  };
  drawForm();
  return formHost;
}

export function renderUnified(context) {
  host = h("div.stack");
  if (state.loading) {
    render(host, h("div.page-head", h("h1", "统一模型")), loading("正在读取统一模型配置。"));
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
      h("div", {}, h("h1", "统一模型"), h("p.sub", "请求统一入口时使用的模型、Key 与回退顺序。")),
      h("div.spacer"),
      state.unified ? badge("已启用", "good") : badge("未启用", "muted"),
      state.revision ? badge(`版本 ${String(state.revision).slice(0, 12)}`, "muted") : null,
    ),
  ];
  if (state.error) children.push(notice(`读取失败: ${state.error}`, "error"));
  if (!state.models.length) {
    children.push(empty("尚未配置可用模型。"));
    render(host, children);
    return;
  }
  if (state.editing) {
    children.push(card(cardHead("当前配置"), editor()));
  } else {
    const unified = state.unified;
    children.push(card(
      cardHead("当前配置", buttonNode("编辑", { small: true, variant: "text", onClick: () => { state.editing = true; draw(); } })),
      kv([
        ["文本模型", unified?.default?.primary?.model || "未配置"],
        ["路由方式", statusText(unified)],
        ["回退模型", unified?.default?.fallback?.model || "未配置"],
        ["图像模型", unified?.image?.primary?.model || "未配置"],
        ["嵌入模型", unified?.embeddings?.primary?.model || "未配置"],
      ]),
    ));
    if (unified) {
      children.push(h("div.btn-row", {},
        buttonNode("停用统一模型", {
          variant: "danger",
          onClick: () => confirmDialog({
            title: "停用统一模型",
            message: "停用统一模型后，统一入口将不再接管请求。是否继续？",
            confirmLabel: "停用",
            danger: true,
            onConfirm: async () => {
              try {
                await api.deleteUnified(state.revision);
                await load();
                toast("统一模型已停用。");
                draw();
              } catch (error) { toast(errorText(error), "error"); }
            },
          }),
        }),
      ));
    }
  }
  render(host, children);
}
