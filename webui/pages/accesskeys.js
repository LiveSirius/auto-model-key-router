// 访问密钥：分发给外部使用者的受限推理凭据。
//
// 这一页取代了原先的「访客模式」。那把固定 key（`amkr-visitor`）的权限是「所有
// allow_visitor 为真的上游 key」的并集：全局共享、无法按人收窄，出了问题也查不到是
// 谁。访问密钥把这件事变成每把 key 自己的两份清单——它能用哪些供应商、能调哪些模型。
//
// 明文 key 只在本页的**两个动作**后出现：新建与轮换。之后列表只显示指纹；要看旧 key
// 只能翻配置文件。凭据随列表一起发出去，等于每次打开这一页都重新泄漏一遍。

import { h, errorText, copyText, truncate } from "../dom.js";
import { api } from "../api.js";
import {
  card, cardHead, notice, badge, empty, loading, render, toast, buttonNode,
  input, field, dialog, confirmDialog, kv,
} from "../ui.js";

const state = {
  keys: [],
  // 可选项来源：供应商 ID 与被授权模型名。清单校验由服务端最终把关（引用不存在的
  // 目标会被 422 拒掉），这里拉全量只是为了给出好选的候选项。
  providerIds: [],
  modelNames: [],
  revision: null,
  loading: true,
  error: null,
  // editing 为密钥 ID 时该行展开成编辑态；creator 为真时弹新建对话框。
  editing: null,
  saving: false,
};

let host = null;

async function load() {
  const [keys, providers, models] = await Promise.all([
    api.accessKeys(), api.providers(), api.models(),
  ]);
  state.keys = keys.access_keys || [];
  state.revision = keys.config_revision;
  state.providerIds = (providers.providers || []).map((provider) => provider.id);
  // 候选模型名 = 真实 ID + 别名。清单按**调用方写的名字**比对（见 config.AccessKeyConfig
  // 的 AllowsModel），所以别名必须一起列出来，否则运维看不到自己常用的那个写法。
  const names = new Set();
  for (const model of models.models || []) {
    names.add(model.id);
    for (const alias of model.aliases || []) names.add(alias);
  }
  state.modelNames = [...names].sort((a, b) => a.localeCompare(b, "zh-CN"));
}

// scopeText 把一份清单渲染成人读的一行。
//
// 三态必须能区分出来，否则「一个都不许」会被显示成「不限制」——那正好是把一条禁令
// 读反。list 为 undefined 表示配置里没写这个字段。
function scopeText(list) {
  if (list === undefined || list === null) return h("span.muted", "不限制");
  if (!list.length) return badge("一个都不许", "bad");
  return h("span.mono", { title: list.join(", ") }, truncate(list.join(", "), 48));
}

// scopeValueOf 把清单还原成提交用的值：undefined（不传）与 null（清除）必须分开。
function scopeValueOf(list) {
  return list === undefined ? null : list;
}

// —— 新建 ——
function openCreate() {
  const nameInput = input({ placeholder: "例如 试用账号 A", required: true });
  const keyInput = input({ placeholder: "留空由服务端生成", autocomplete: "off" });
  const providersInput = input({ placeholder: "逗号分隔供应商 ID，留空表示不限制" });
  const modelsInput = input({ placeholder: "逗号分隔模型名或别名，留空表示不限制" });
  const errorHost = h("div");
  let ref = null;
  let created = null;

  const parseList = (value) => {
    const items = value.split(",").map((item) => item.trim()).filter(Boolean);
    // 空输入 = 不传该字段 = 不限制；有输入但全是逗号也给空数组（显式「一个都不许」），
    // 交给服务端按清单校验，不在这里替用户猜。
    return value.trim() === "" ? undefined : items;
  };

  const submit = async () => {
    const name = nameInput.value.trim();
    if (!name) return;
    state.saving = true;
    errorHost.replaceChildren();
    try {
      const result = await api.createAccessKey(state.revision, name, {
        key: keyInput.value.trim() || undefined,
        providers: parseList(providersInput.value),
        models: parseList(modelsInput.value),
      });
      created = result;
      await load();
      toast(`访问密钥 ${name} 已创建。`);
    } catch (error) {
      render(errorHost, notice(`创建失败: ${errorText(error)}`, "error"));
      state.saving = false;
      return;
    }
    state.saving = false;
    // 建好后换一屏显示明文：这是**唯一**能拿到它的时刻，直接用 toast 一闪而过
    // 等于让用户永远拿不到钥匙。
    ref.close();
    showPlaintext(created, "访问密钥已创建");
    draw();
  };

  const body = h("div.stack", {},
    field("名称", nameInput),
    field("密钥（留空自动生成）", keyInput),
    field("允许的供应商", providersInput),
    field("允许的模型", modelsInput),
    h("p.muted", "两份清单都留空表示不限制。清单按调用方写的模型名比对，因此别名也可以填。"
      + "密钥只写入服务端，之后列表只显示指纹。"),
    errorHost,
  );
  ref = dialog({
    title: "新建访问密钥",
    body,
    actions: [
      { label: "取消", variant: "text", onClick: () => ref.close() },
      { label: "创建", onClick: submit },
    ],
  });
}

// —— 明文展示 ——
// 新建与轮换共用：两者的共同点是「刚生成的明文只在这一刻可见」。
function showPlaintext(result, title) {
  const value = result?.key || "";
  const area = h("textarea.copy-area", { readonly: true, rows: 3 }, value);
  const ref = dialog({
    title,
    closeOnBackdrop: false,
    body: h("div.stack", {},
      notice("这是唯一一次显示明文密钥。关闭后只能看到指纹，需要再取请轮换。", "warn"),
      area,
      kv([
        ["名称", result?.name || "-"],
        ["ID", h("code.mono", result?.id || "-")],
      ]),
    ),
    actions: [
      {
        label: "复制",
        onClick: async () => {
          try {
            await copyText(value);
            toast("已复制到剪贴板。");
          } catch (error) {
            toast(errorText(error), "error");
          }
        },
      },
      { label: "完成", onClick: () => ref.close() },
    ],
  });
}

// —— 编辑（名字 / 启停 / 两份清单）——
function editorRow(key) {
  const nameInput = input({ value: key.name, required: true });
  const providersInput = input({
    value: (key.providers || []).join(", "),
    placeholder: "逗号分隔，留空表示不限制",
  });
  const modelsInput = input({
    value: (key.models || []).join(", "),
    placeholder: "逗号分隔，留空表示不限制",
  });
  const enabledInput = h("input", { type: "checkbox", checked: key.enabled });
  const errorHost = h("div");

  // 编辑态的两个复选框：勾上=「显式写这份清单」，不勾=「清除清单回到不限制」。
  // 这与 specAccessKeyUpdate 的必填三态一一对应——不传字段会被当成「漏传」而被拒。
  const providerEnabled = h("input", { type: "checkbox", checked: key.providers !== undefined });
  const modelEnabled = h("input", { type: "checkbox", checked: key.models !== undefined });

  const listOf = (enabledInput2, textInput) => (enabledInput2.checked
    ? textInput.value.split(",").map((item) => item.trim()).filter(Boolean)
    : null);

  const save = async () => {
    state.saving = true;
    errorHost.replaceChildren();
    try {
      await api.updateAccessKey(state.revision, key.id, {
        name: nameInput.value.trim(),
        enabled: enabledInput.checked,
        providers: listOf(providerEnabled, providersInput),
        models: listOf(modelEnabled, modelsInput),
      });
      await load();
      state.editing = null;
      toast("访问密钥已更新。");
      draw();
    } catch (error) {
      render(errorHost, notice(`保存失败: ${errorText(error)}`, "error"));
      // 409 是配置版本过期：重新取一遍，让用户看到当前状态再改。
      if (error.status === 409) { await load().catch(() => {}); draw(); }
    }
    state.saving = false;
  };

  return h("tr", {}, h("td", { colspan: "5" },
    h("div.stack", {},
      h("div.form-grid", {},
        field("名称", nameInput),
        field("启用", h("label.check", enabledInput, enabledInput.checked ? "已启用" : "已停用")),
      ),
      h("div.form-grid", {},
        field(h("label.check", providerEnabled, "限制供应商"), providersInput),
        field(h("label.check", modelEnabled, "限制模型"), modelsInput),
      ),
      h("p.muted", "不勾选=清除该清单（回到不限制）；勾选但留空=一个都不许。"),
      errorHost,
      h("div.btn-row", {},
        buttonNode("保存", { disabled: state.saving, onClick: save }),
        buttonNode("取消", { variant: "text", onClick: () => { state.editing = null; draw(); } }),
      ),
    ),
  ));
}

// —— 行 ——
// 手写表格而不是用 table()：编辑态要把**整行**换成一张表单（colspan 铺满），而
// table() 的列渲染是逐单元格的，给不出跨列的行。手写这点代价换来的是一处结构清晰。
function keyRow(key) {
  if (state.editing === key.id) return editorRow(key);
  return h("tr", {},
    h("td", {},
      h("div.stack.tight", {},
        h("strong", key.name),
        h("code.mono", { title: key.id }, truncate(key.id, 16)),
      ),
    ),
    h("td", {}, h("span.inline", {},
      badge(key.enabled ? "已启用" : "已停用", key.enabled ? "good" : "muted"),
      h("code.mono", key.key_fingerprint || "-"),
    )),
    h("td", {}, scopeText(key.providers)),
    h("td", {}, scopeText(key.models)),
    h("td", {}, actionButtons(key)),
  );
}

function actionButtons(key) {
  return h("div.btn-row", {},
    buttonNode("编辑", {
      small: true, variant: "secondary",
      onClick: () => { state.editing = key.id; draw(); },
    }),
    buttonNode("轮换", {
      small: true, variant: "secondary",
      onClick: () => confirmDialog({
        title: "轮换访问密钥",
        message: `将换掉「${key.name}」的密钥，旧密钥立刻失效。请准备把新密钥交给使用者。`,
        confirmLabel: "轮换",
        onConfirm: () => rotate(key),
      }),
    }),
    buttonNode("删除", {
      small: true, variant: "danger",
      onClick: () => confirmDialog({
        title: "删除访问密钥",
        message: `将删除「${key.name}」，使用它的调用方会立刻收到 401。此操作不可撤销。`,
        confirmLabel: "删除",
        danger: true,
        onConfirm: () => remove(key),
      }),
    }),
  );
}

// keyTable 渲染整张表（含表头），行来自 state.keys。
function keyTable() {
  return h("div.table-scroll", {},
    h("table.table", {},
      h("thead", {}, h("tr", {},
        h("th", "名称"),
        h("th", "状态 / 指纹"),
        h("th", "允许的供应商"),
        h("th", "允许的模型"),
        h("th", "操作"),
      )),
      h("tbody", {}, state.keys.map((key) => keyRow(key))),
    ),
  );
}

async function rotate(key) {
  try {
    const result = await api.rotateAccessKey(state.revision, key.id);
    await load();
    showPlaintext({ ...result, name: key.name }, "访问密钥已轮换");
    draw();
  } catch (error) {
    toast(`轮换失败: ${errorText(error)}`, "error");
    if (error.status === 409) { await load().catch(() => {}); draw(); }
  }
}

async function remove(key) {
  try {
    await api.deleteAccessKey(state.revision, key.id);
    await load();
    toast(`访问密钥 ${key.name} 已删除。`);
    draw();
  } catch (error) {
    toast(`删除失败: ${errorText(error)}`, "error");
    if (error.status === 409) { await load().catch(() => {}); draw(); }
  }
}

function draw() {
  if (!host) return;
  const children = [
    h("div.page-head", {},
      h("div.page-title", {},
        h("h1", "访问密钥"),
        h("p.sub", "分发给外部使用者的受限凭据。每把密钥限定可用哪些供应商与哪些模型。"),
      ),
      h("div.page-actions", {},
        buttonNode("新建访问密钥", { iconName: "key", onClick: openCreate }),
      ),
    ),
  ];

  if (state.error) children.push(notice(`读取失败：${state.error}`, "error"));

  if (state.loading) {
    children.push(card(cardHead("访问密钥"), loading("正在读取…")));
    render(host, children);
    return;
  }

  children.push(card(
    cardHead("访问密钥",
      badge(`${state.keys.length} 把`, "muted"),
    ),
    state.keys.length
      ? keyTable()
      : empty("还没有访问密钥。", {
          icon: "key",
          hint: "新建一把并把它交给使用者，它只能调用你在这把密钥上列出的供应商与模型。",
        }),
    h("div.card-foot", {},
      "明文密钥只在创建与轮换时显示一次。需要重新获取请轮换——旧密钥会立刻失效。"
      + "未配置清单表示不限制；配成空清单表示一个都不许。"),
  ));

  render(host, children);
}

// render 必须是**同步**的：app.js 直接把它返回的节点 mount 进 #content，写成 async
// 就是返回一个 Promise，会被当成子节点渲染成整页的 "[object Promise]"。取数在后台
// 完成后自行重绘，与其它页面同一约定。
export function renderAccessKeys(context) {
  host = h("div.stack");
  state.loading = true;
  state.error = null;
  draw();
  load()
    .catch((error) => { state.error = errorText(error); })
    .finally(() => { state.loading = false; draw(); });
  return host;
}
