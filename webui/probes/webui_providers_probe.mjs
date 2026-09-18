// 供应商页回归探针：用最小 DOM 垫片驱动真实的 webui/pages/providers.js 与 brand-icons.js。
// 用法：node webui_providers_probe.mjs，结果以 JSON 打到 stdout，失败时退出码 1。
//
// 为什么必须用 DOM 垫片：这次改动的全部风险都在"渲染出来的结构"上——供应商切换器
// 是否真的竖排在左侧、品牌图标是否真的落到按钮里、预置是否真的填进了输入框。
// brand-icons.js 的纯函数（brandForProvider）测不到这些。
//
// 垫片忠实地保留 null 与文本节点语义（原生 replaceChildren 会把 null 参数变成文本
// "null"），否则会把 dom.js 的回归藏起来——见 webui_tip_probe.mjs 的同款说明。

import { pathToFileURL } from "node:url";
import path from "node:path";

const WEBUI = path.resolve(import.meta.dirname, "..");

class FakeNode {
  constructor(tag) {
    this.tagName = tag;
    this.children = [];
    this.attrs = {};
    this.listeners = {};
    this.className = "";
    this.style = {};
    this.parent = null;
    this.value = "";
    this.checked = false;
  }
  // 与 webui/dom.js 的 append() 对齐：它自己过滤 null，再交给这里。
  append(...nodes) { this.#adopt(nodes); }
  // 忠实还原浏览器：null / undefined / false 都会变成文本节点。
  replaceChildren(...nodes) { this.children = []; this.#adopt(nodes); }
  #adopt(nodes) {
    for (const node of nodes.flat()) {
      const child = node instanceof FakeNode ? node : new FakeText(node);
      child.parent = this;
      this.children.push(child);
    }
  }
  appendChild(node) { this.append(node); return node; }
  setAttribute(key, value) { this.attrs[key] = String(value); }
  getAttribute(key) { return this.attrs[key]; }
  addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); }
  removeEventListener(type, fn) {
    this.listeners[type] = (this.listeners[type] || []).filter((item) => item !== fn);
  }
  remove() {
    if (this.parent) this.parent.children = this.parent.children.filter((c) => c !== this);
  }
  // dialog() 用它把焦点放到第一个控件上；只支持 "a, b, c" 这样的标签名列表，
  // 对本次断言够用，也避免引入真正的选择器实现。
  querySelector(selector) {
    const tags = String(selector).split(",").map((part) => part.trim());
    return findAll(this, (node) => tags.includes(node.tagName))[0] || null;
  }
  prepend(...nodes) { this.children.unshift(...nodes.flat()); }
  replaceWith() {}
  focus() {}
  get classList() {
    const self = this;
    const names = () => String(self.className).split(/\s+/).filter(Boolean);
    return {
      add: (...add) => { self.className = [...new Set([...names(), ...add])].join(" "); },
      remove: (...drop) => { self.className = names().filter((n) => !drop.includes(n)).join(" "); },
      contains: (name) => names().includes(name),
    };
  }
  get textContent() {
    return this.children
      .map((c) => (c.textContent === undefined ? String(c) : c.textContent))
      .join(" ");
  }
}

// 文本节点必须也是 Node（dom.js 用 `child instanceof Node` 判断），否则会被
// 当成字符串再包一层，读出来就是 "[object Object]"。
class FakeText extends FakeNode {
  constructor(data) { super("#text"); this.data = String(data); }
  get textContent() { return this.data; }
}

global.Node = FakeNode;
global.document = {
  createElement: (tag) => new FakeNode(tag),
  createElementNS: (_ns, tag) => new FakeNode(tag),
  createTextNode: (text) => new FakeText(text),
  body: new FakeNode("body"),
  addEventListener() {},
  removeEventListener() {},
};

// —— 假的供应商配置：覆盖"认得出品牌"与"认不出"两种情况 ——
const PROVIDERS = [
  { id: "openai", base_url: "https://api.openai.com", keys: [{ name: "primary" }] },
  { id: "my-deepseek", base_url: "https://api.deepseek.com", keys: [] },
  { id: "local-lab", base_url: "https://gateway.internal.example", keys: [{ name: "a" }, { name: "b" }] },
];

global.fetch = async (url) => {
  const target = String(url);
  let payload = {};
  if (target.includes("/api/providers")) {
    payload = { providers: PROVIDERS, config_revision: "rev-000000000000" };
  }
  return { ok: true, status: 200, async text() { return JSON.stringify(payload); } };
};
global.localStorage = { getItem: () => null, setItem() {}, removeItem() {} };
global.location = { pathname: "/ui/", hash: "#/providers" };
global.window = { addEventListener() {}, isSecureContext: true, location: global.location };

const { renderProviders } = await import(pathToFileURL(path.join(WEBUI, "pages", "providers.js")).href);
const { PROVIDER_PRESETS, brandForProvider } = await import(pathToFileURL(path.join(WEBUI, "brand-icons.js")).href);

const checks = {};
const check = (name, ok, detail = "") => {
  checks[name] = ok ? true : `FAILED${detail ? `: ${detail}` : ""}`;
};
const findAll = (node, predicate, out = []) => {
  if (predicate(node)) out.push(node);
  for (const child of node.children) findAll(child, predicate, out);
  return out;
};
// SVG 节点的 class 落在属性上（SVGElement.className 只读，dom.js 走 setAttribute）。
const hasClass = (node, name) =>
  [node.className, node.attrs?.class].some((value) =>
    String(value || "").split(/\s+/).includes(name));
const byClass = (root, name) => findAll(root, (n) => hasClass(n, name));
const buttonWithText = (root, text) =>
  findAll(root, (n) => n.tagName === "button" && n.textContent.includes(text))[0];
const click = (node) => { for (const fn of node.listeners.click || []) fn({ target: node, preventDefault() {} }); };

// —— 渲染真实页面 ——
const host = renderProviders({});
await new Promise((resolve) => setTimeout(resolve, 0));
await new Promise((resolve) => setTimeout(resolve, 0));

// —— 布局：切换器必须竖排在左侧，横向标签页条必须消失 ——
const split = byClass(host, "provider-split")[0];
const rail = byClass(host, "provider-rail")[0];
check("split_present", Boolean(split));
check("rail_is_nav", rail?.tagName === "nav", rail?.tagName);
check("rail_is_first_column", split?.children[0] === rail);
check("detail_is_second_column", hasClass(split?.children[1] || new FakeNode("x"), "provider-detail"));
check("old_tabs_strip_removed", byClass(host, "tabs").length === 0);
check("detail_holds_cards", byClass(split?.children[1] || new FakeNode("x"), "card").length >= 2);

// —— 导航项：每个供应商一条，名称与 Key 数都要在 ——
const items = byClass(rail || new FakeNode("x"), "rail-item");
check("rail_item_count", items.length === PROVIDERS.length, `${items.length} != ${PROVIDERS.length}`);
check("rail_marks_active", items[0]?.attrs["aria-current"] === "true", items[0]?.attrs["aria-current"]);
check("rail_others_not_current", items.slice(1).every((n) => n.attrs["aria-current"] === undefined));
check("rail_shows_key_count", items[0]?.textContent.includes("1 Key") === true, items[0]?.textContent);
check("rail_marks_no_key", items[1]?.textContent.includes("无 Key") === true, items[1]?.textContent);
check("rail_semantics_not_fake_tab", items.every((n) => n.attrs.role === undefined));

// —— 品牌图标：认得出的用品牌标志，认不出的回退到通用图标 ——
check("brand_icon_used_for_openai", byClass(items[0] || new FakeNode("x"), "brand-icon").length === 1);
check("brand_icon_used_for_renamed_provider", byClass(items[1] || new FakeNode("x"), "brand-icon").length === 1);
check("generic_icon_for_unknown_host",
  byClass(items[2] || new FakeNode("x"), "brand-icon").length === 0 &&
  byClass(items[2] || new FakeNode("x"), "icon").length === 1);
// 品牌图标是填充路径且继承 currentColor；这两点是它和描边图标共存的约定。
const brandSvg = byClass(host, "brand-icon")[0];
check("brand_icon_fills_currentcolor", brandSvg?.attrs.fill === "currentColor", brandSvg?.attrs.fill);
check("brand_icon_viewbox_24", brandSvg?.attrs.viewBox === "0 0 24 24", brandSvg?.attrs.viewBox);
check("brand_icon_has_path", byTag(brandSvg || new FakeNode("x"), "path").length > 0);

// —— 切换供应商：详情必须跟着换 ——
click(items[1]);
const rail2 = byClass(host, "provider-rail")[0];
check("switch_updates_current", byClass(rail2, "rail-item")[1]?.attrs["aria-current"] === "true");
check("switch_repaints_detail", findText(host, "my-deepseek"));

function byTag(root, tag) {
  return findAll(root, (node) => node.tagName === tag);
}
// 按整棵子树的文本比对，而不是只看叶子：文本落在 FakeText 上，承载它的元素
// （h3 / span）本身还有一层子节点，用"无子节点"当叶子判据会漏掉它们。
function findText(root, needle) {
  return findAll(root, (node) => node.tagName !== "#text" && node.textContent === needle).length > 0;
}

// —— 预置：必须在"添加供应商"对话框里，点击即填名称与地址 ——
const addButton = buttonWithText(host, "添加供应商");
check("add_button_present", Boolean(addButton));
click(addButton);

const presets = byClass(document.body, "preset");
check("presets_rendered_in_dialog", presets.length === PROVIDER_PRESETS.length,
  `${presets.length} != ${PROVIDER_PRESETS.length}`);
check("presets_are_buttons", presets.every((n) => n.tagName === "button"));
check("presets_have_icons", presets.every((n) => byClass(n, "brand-icon").length === 1));
check("presets_have_label", presets.every((n) => byClass(n, "brand-icon").length && n.textContent.trim().length > 0));
check("preset_group_labelled",
  byClass(document.body, "preset-grid")[0]?.attrs["aria-label"] === "常见供应商");

const dialogPanel = byClass(document.body, "dialog")[0];
const inputs = byClass(dialogPanel || new FakeNode("x"), "input");
check("dialog_has_name_and_url_inputs", inputs.length === 2, `${inputs.length} 个输入框`);

// 点第二个预置（DeepSeek），断言它把名称与地址都填了进去。
click(presets[1]);
check("preset_fills_name", inputs[0]?.value === PROVIDER_PRESETS[1].id, inputs[0]?.value);
check("preset_fills_base_url", inputs[1]?.value === PROVIDER_PRESETS[1].baseUrl, inputs[1]?.value);
check("preset_marks_pressed", presets[1]?.attrs["aria-pressed"] === "true", presets[1]?.attrs["aria-pressed"]);
check("other_presets_not_pressed", presets[0]?.attrs["aria-pressed"] === "false", presets[0]?.attrs["aria-pressed"]);

// —— 预置地址必须与后端默认路由拼得起来 ——
// 后端把默认路径（v1/chat/completions 一类）拼在 base_url 之后，所以 base_url 里
// 不能再带 /v1 或具体端点，否则会拼出 /v1/v1/... 这种死地址。这条不变量是预置
// 功能真正的风险点：拼错了用户只会看到探测失败，很难想到是预置地址的问题。
const join = (base, p) => `${String(base).replace(/\/+$/, "")}/${String(p).replace(/^\/+/, "")}`;
const badBases = PROVIDER_PRESETS.filter((p) => /\/v\d+$/.test(p.baseUrl) || /\/(chat\/completions|messages|responses)$/.test(p.baseUrl));
check("preset_bases_have_no_version_suffix", badBases.length === 0, badBases.map((p) => p.id).join(","));
check("preset_join_default_path",
  PROVIDER_PRESETS.every((p) => join(p.baseUrl, "v1/chat/completions").endsWith("/v1/chat/completions")));
check("preset_ids_unique", new Set(PROVIDER_PRESETS.map((p) => p.id)).size === PROVIDER_PRESETS.length);
check("preset_bases_unique", new Set(PROVIDER_PRESETS.map((p) => p.baseUrl)).size === PROVIDER_PRESETS.length);
check("preset_hosts_unique", new Set(PROVIDER_PRESETS.map((p) => p.host)).size === PROVIDER_PRESETS.length);
check("preset_count_reasonable", PROVIDER_PRESETS.length >= 8 && PROVIDER_PRESETS.length <= 24, String(PROVIDER_PRESETS.length));

// —— 品牌识别：host 优先于 id，且不得被单字符品牌名误伤 ——
// xAI 的品牌名是单字符 "x"。若拿 brand 当子串去匹配供应商 id，`my-box`、
// `proxy` 这类无关命名都会被判成 xAI——所以匹配必须走 preset.keys。
check("brand_by_host", brandForProvider("任意改名", "https://api.deepseek.com") === "deepseek");
check("brand_by_host_subdomain", brandForProvider("x", "https://api.moonshot.cn") === "moonshot");
check("brand_by_id_substring", brandForProvider("my-deepseek", "https://gateway.internal.example") === "deepseek");
check("brand_by_alias", brandForProvider("claude-main", "https://gateway.internal.example") === "anthropic");
check("brand_host_beats_id", brandForProvider("deepseek", "https://api.openai.com") === "openai");
check("brand_no_single_char_false_positive",
  brandForProvider("my-box", "https://gateway.internal.example") === null,
  String(brandForProvider("my-box", "https://gateway.internal.example")));
check("brand_unknown_is_null", brandForProvider("local-lab", "https://gateway.internal.example") === null);
// 本地三家的 host 靠端口区分，所以端口必须留在 host 里参与比对。
check("brand_local_by_port", brandForProvider("local", "http://127.0.0.1:11434") === "ollama");
check("brand_handles_garbage_url", brandForProvider("vllm", "::not a url::") === "vllm");
check("brand_trailing_slash_ok", brandForProvider("x", "https://api.groq.com/openai/") === "groq");

const failed = Object.entries(checks).filter(([, value]) => value !== true);
console.log(JSON.stringify({ checks, failed: failed.length }, null, 2));
if (failed.length) {
  console.error(`\n${failed.length} 项断言失败`);
  process.exit(1);
}
console.log(`\n全部 ${Object.keys(checks).length} 项断言通过`);
