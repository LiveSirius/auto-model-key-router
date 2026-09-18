// 任务路由：把「模型 + 固定采样参数」打包成一个可直接当 model 传的任务名。
//
// 调用方传 model: "TASK_XXXXXX" 即可命中；因为参数由任务固定，调用方再传
// temperature 这类采样参数会被服务端拒绝（reasoning_effort 例外，见后端说明）。

import { h, mount, errorText } from "../dom.js";
import { api } from "../api.js";
import { card, cardHead, notice, badge, empty, loading, render, toast, buttonNode, input, select, confirmDialog, kv } from "../ui.js";

const EFFORTS = [
  { value: "", label: "不固定" },
  { value: "none", label: "none" },
  { value: "minimal", label: "minimal" },
  { value: "low", label: "low" },
  { value: "medium", label: "medium" },
  { value: "high", label: "high" },
  { value: "xhigh", label: "xhigh" },
  { value: "max", label: "max" },
];

// 数值型固定参数：标签 + 是否整数（top_k / seed 必须是整数）。
const NUMERIC_PARAMS = [
  { key: "temperature", label: "temperature" },
  { key: "top_p", label: "top_p" },
  { key: "top_k", label: "top_k", integer: true },
  { key: "frequency_penalty", label: "frequency_penalty" },
  { key: "presence_penalty", label: "presence_penalty" },
  { key: "seed", label: "seed", integer: true },
];

const PARAM_LABELS = Object.fromEntries([
  ...NUMERIC_PARAMS.map((param) => [param.key, param.label]),
  ["stop", "stop"],
  ["reasoning_effort", "reasoning_effort"],
]);

const state = { tasks: [], models: [], revision: null, loading: true, error: null, editing: null, saving: false };

let host = null;
// 首屏会被连续渲染两次（挂载 + 首次指标刷新），于是有两个 load 在途。没有这个
// 令牌的话，先发的请求晚回来时会重绘整个页面：用户此时若已经点开编辑器，正在
// 手填的参数就被悄悄冲掉了。
let paintToken = 0;

async function load() {
  const [tasks, models] = await Promise.all([api.tasks(), api.models()]);
  state.tasks = tasks.tasks || [];
  state.revision = tasks.config_revision ?? models.config_revision;
  state.models = models.models || [];
}

function summary(task) {
  const names = Object.keys(task.params || {});
  return names.length ? names.map((name) => PARAM_LABELS[name] || name).join("、") : "全部透传";
}

// 逗号分隔的 stop 序列；空字符串表示不固定该参数。
// task 为 null 表示"新建"（taskEditor(null)），因此必须对 task 本身做可选链：
// 只写 task.params?.stop 会在新建时抛 TypeError，让整个编辑器画不出来。
const stopText = (task) => (task?.params?.stop || []).join(", ");

function taskEditor(task) {
  const isNew = !task;
  const nameInput = input({
    value: task?.name || "",
    placeholder: "TASK_000001",
    disabled: !isNew || state.saving,
  });
  let model = task?.model || state.models[0]?.id || "";
  let fallback = task?.fallback_model || "";

  // 备选下拉要排掉首选（两者不能相同）。刷新它**不能靠重绘整页**：重绘会连下面
  // 那些手填的参数输入框一起重建，用户刚敲的数字就没了，所以只就地换它的选项。
  // 首选下拉始终列出全部模型，无需刷新。
  const modelOptions = () => state.models.map((item) => ({ value: item.id, label: item.id }));
  const fallbackOptions = (value) => [{ value: "", label: "不启用备选" }].concat(
    modelOptions().filter((option) => option.value !== value),
  );

  const fallbackSelect = select(fallbackOptions(model), { value: fallback, disabled: state.saving });
  const primarySelect = select(modelOptions(), {
    value: model,
    disabled: state.saving,
    onChange: (event) => {
      model = event.target.value;
      // 首选换成了当前的备选时清掉备选，避免两者相同。
      if (fallback === model) fallback = "";
      mount(fallbackSelect, ...fallbackOptions(model).map((option) =>
        h("option", { value: option.value, selected: option.value === fallback }, option.label)));
    },
  });
  fallbackSelect.addEventListener("change", (event) => { fallback = event.target.value; });

  const numberInputs = {};
  for (const param of NUMERIC_PARAMS) {
    const value = task?.params?.[param.key];
    numberInputs[param.key] = input({
      value: value === undefined || value === null ? "" : String(value),
      placeholder: "留空表示不固定",
      inputmode: "decimal",
      disabled: state.saving,
    });
  }
  const stopInput = input({
    value: stopText(task),
    placeholder: "逗号分隔，留空表示不固定",
    disabled: state.saving,
  });
  const effortSelect = select(EFFORTS, { value: task?.params?.reasoning_effort || "", disabled: state.saving });

  const errorHost = h("div");
  // 保存期间要锁住的控件：请求发出后用户再改也不会被带上，与其让人以为改了，
  // 不如先禁用。任务名不在其中——编辑既有任务时它本来就一直是只读的。
  const lockable = [primarySelect, fallbackSelect, effortSelect, stopInput, ...Object.values(numberInputs)];

  // 只读取用户填过的参数；留空 = 不写进配置，调用方可以自己传。
  const collectParams = () => {
    const params = {};
    for (const param of NUMERIC_PARAMS) {
      const raw = numberInputs[param.key].value.trim();
      if (!raw) continue;
      const value = Number(raw);
      if (!Number.isFinite(value)) throw new Error(`${param.label} 必须是数字`);
      if (param.integer && !Number.isInteger(value)) throw new Error(`${param.label} 必须是整数`);
      params[param.key] = value;
    }
    const stop = stopInput.value.split(",").map((item) => item.trim()).filter(Boolean);
    if (stop.length) params.stop = stop;
    if (effortSelect.value) params.reasoning_effort = effortSelect.value;
    return params;
  };

  // 保存时只改按钮自己的状态，不重绘表单：重绘会丢掉用户刚填的参数，也会把
  // 下面的 errorHost 从文档里摘掉，于是报错写进一个没人看得见的节点。
  const cancelButton = buttonNode("取消", {
    variant: "text",
    onClick: () => { state.editing = null; draw(); },
  });
  const saveButton = buttonNode("保存任务", {
    variant: "primary",
    onClick: async () => {
      let params;
      try {
        params = collectParams();
      } catch (error) {
        render(errorHost, notice(error.message, "error"));
        return;
      }
      const name = isNew ? nameInput.value.trim() : task.name;
      if (!name) { render(errorHost, notice("请填写任务名。", "error")); return; }
      if (!model) { render(errorHost, notice("请选择首选模型。", "error")); return; }
      if (fallback && fallback === model) { render(errorHost, notice("备选模型不能与首选模型相同。", "error")); return; }

      render(errorHost);
      state.saving = true;
      saveButton.disabled = true;
      // 保存途中不许取消或改表单：请求已经在路上，此时丢掉编辑器只会让人以为没保存。
      cancelButton.disabled = true;
      for (const node of lockable) node.disabled = true;
      mount(saveButton, "保存中…");
      try {
        if (isNew) {
          await api.createTask(state.revision, { name, model, fallback_model: fallback || null, params });
        } else {
          await api.updateTask(state.revision, name, { model, fallback_model: fallback || null, params });
        }
        await load();
        state.editing = null;
        state.saving = false;
        toast("任务路由已保存。");
        draw();
        return;
      } catch (error) {
        render(errorHost, notice(`保存失败: ${errorText(error)}`, "error"));
        // 版本冲突说明手上的配置已过期，重新读一次再让用户重试。
        if (error.status === 409) await load().catch(() => {});
      }
      state.saving = false;
      saveButton.disabled = false;
      cancelButton.disabled = false;
      for (const node of lockable) node.disabled = false;
      mount(saveButton, "保存任务");
    },
  });

  return h("div.stack", {},
    h("div.form-grid", {},
      h("label.field", h("span", "任务名"), nameInput),
      h("label.field", h("span", "首选模型"), primarySelect),
      h("label.field", h("span", "备选模型"), fallbackSelect),
      h("label.field", h("span", "推理强度"), effortSelect),
    ),
    h("p.muted", "任务名就是调用方传的 model。任务名不能与模型 ID、别名或隐藏别名撞名。"),
    h("h4", "固定采样参数"),
    h("p.muted", "填了的参数由任务说了算：调用方再传同名参数会被直接拒绝。留空的参数照常透传，max_tokens 一类不在列表里的参数也始终透传。"),
    h("div.form-grid", {}, NUMERIC_PARAMS.map((param) => h("label.field", h("span", param.label), numberInputs[param.key]))),
    h("label.field", h("span", "stop（多个用逗号分隔）"), stopInput),
    errorHost,
    h("div.btn-row", {},
      saveButton,
      cancelButton,
    ),
  );
}

export function renderTasks(context) {
  host = h("div.stack");
  if (state.loading) {
    render(host, h("div.page-head", h("h1", "任务路由")), loading("正在读取任务路由。"));
    const token = ++paintToken;
    (async () => {
      try { await load(); state.error = null; } catch (error) { state.error = errorText(error); }
      // 已有更新的一轮在读，让那轮负责落笔。
      if (token !== paintToken) return;
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
      h("div", {}, h("h1", "任务路由"),
        h("p.sub", "把任务名当作模型名调用，自动使用该任务固定的模型与采样参数。")),
      h("div.spacer"),
      state.revision ? badge(`版本 ${String(state.revision).slice(0, 12)}`, "muted") : null,
      buttonNode("新建任务", {
        small: true,
        variant: "secondary",
        disabled: state.saving || !state.models.length,
        onClick: () => { state.editing = "__new__"; draw(); },
      }),
    ),
  ];
  if (state.error) children.push(notice(`无法读取或写入任务路由: ${state.error}`, "error"));
  if (!state.models.length) {
    children.push(empty("尚未配置可用模型。", { hint: "请先在供应商页添加 Key 并绑定服务模型，再建立任务路由。" }));
    render(host, children);
    return;
  }

  if (state.editing === "__new__") {
    children.push(card(cardHead("新建任务"), taskEditor(null)));
  }

  if (!state.tasks.length && state.editing !== "__new__") {
    children.push(empty("尚未配置任务路由。", {
      hint: "新建一个任务后，调用方传 model: \"TASK_XXXXXX\" 即可命中。",
      action: buttonNode("新建任务", { variant: "secondary", onClick: () => { state.editing = "__new__"; draw(); } }),
    }));
    render(host, children);
    return;
  }

  for (const task of state.tasks) {
    if (state.editing === task.name) {
      children.push(card(cardHead(`编辑 · ${task.name}`), taskEditor(task)));
      continue;
    }
    children.push(card(
      cardHead(task.name,
        badge(task.fallback_model ? `备选 ${task.fallback_model}` : "无备选", "muted"),
        buttonNode("编辑", { small: true, variant: "text", disabled: state.saving, onClick: () => { state.editing = task.name; draw(); } }),
        buttonNode("删除", {
          small: true,
          variant: "text",
          disabled: state.saving,
          onClick: () => confirmDialog({
            title: "删除任务路由",
            message: `删除任务 ${task.name}？调用方再传这个名字会被当作未配置的模型。`,
            confirmLabel: "删除",
            danger: true,
            onConfirm: async () => {
              try {
                await api.deleteTask(state.revision, task.name);
                await load();
                state.editing = null;
                toast("任务路由已删除。");
              } catch (error) { toast(errorText(error), "error"); }
              draw();
            },
          }),
        }),
      ),
      kv([
        ["首选模型", task.model],
        ["备选模型", task.fallback_model || "未配置"],
        ["固定参数", summary(task)],
      ]),
    ));
  }
  render(host, children);
}
