// 模型路由页回归探针：用最小 DOM 垫片驱动真实的 webui/pages/routing.js。
// 用法：node webui_routing_probe.mjs，结果以 JSON 打到 stdout，失败时退出码 1。
//
// 为什么必须用 DOM 垫片：这次改动的全部风险都在"渲染出来的结构"上——模型列表是否
// 真的竖排在左侧、旧的横排标签页条是否真的消失、切换模型时右侧详情是否跟着换。
// routeEditor 里的纯逻辑（候选 Key 去重等）在别处已被覆盖，测不到这些。
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
  querySelector() { return null; }
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

// —— 假的模型路由配置 ——
// 三条路由覆盖两种详情形态：有多个目标 + 别名（route-a）、只有一个目标、以及
// 零目标（route-c）。零目标是最容易在"按顺序排列"这类文案上出错的边界。
const ROUTES = [
  {
    id: "gpt-5.5",
    aliases: ["gpt", "gpt-latest"],
    hidden_aliases: ["gpt-internal"],
    routing_mode: "round_robin",
    targets: [
      { provider: "openai", key: "primary", upstream_model: "gpt-5.5" },
      { provider: "openai", key: "backup", upstream_model: "gpt-5.5-2026" },
    ],
  },
  {
    id: "claude-sonnet-4-5",
    aliases: [],
    hidden_aliases: [],
    routing_mode: null,
    targets: [{ provider: "anthropic", key: "main", upstream_model: "claude-sonnet-4-5" }],
  },
  { id: "local-llama", aliases: [], hidden_aliases: [], routing_mode: "only_first", targets: [] },
];

const PROVIDERS = [
  {
    id: "openai",
    base_url: "https://api.openai.com",
    keys: [
      { name: "primary", capabilities: { models: ["gpt-5.5"] } },
      // secondary 也探测到 gpt-5.5 但**未绑定**：候选列表该有它、不该有 primary。
      { name: "secondary", capabilities: { models: ["gpt-5.5"] } },
    ],
  },
  {
    id: "anthropic",
    base_url: "https://api.anthropic.com",
    keys: [{ name: "main", capabilities: { models: ["claude-sonnet-4-5"] } }],
  },
];

global.fetch = async (url) => {
  const target = String(url);
  let payload = {};
  if (target.includes("/api/routes")) {
    payload = { routes: ROUTES, config_revision: "rev-000000000000" };
  } else if (target.includes("/api/providers")) {
    payload = { providers: PROVIDERS, config_revision: "rev-000000000000" };
  }
  return { ok: true, status: 200, async text() { return JSON.stringify(payload); } };
};
global.localStorage = { getItem: () => null, setItem() {}, removeItem() {} };
global.location = { pathname: "/ui/", hash: "#/routing" };
global.window = { addEventListener() {}, isSecureContext: true, location: global.location };

const { renderRouting } = await import(pathToFileURL(path.join(WEBUI, "pages", "routing.js")).href);

const checks = {};
const check = (name, ok, detail = "") => {
  checks[name] = ok ? true : `FAILED${detail ? `: ${detail}` : ""}`;
};
// 收尾：报告并决定退出码。抽成函数是因为下面有一条"结构不对就别再驱动"的早退——
// 旧实现（横排标签页）会在这里被判出来，此时若继续点按钮，第一个 undefined 会抛
// TypeError，把已经查实的布局失败淹没在栈里，读的人只看到"探针自己崩了"。
function report() {
  const failed = Object.entries(checks).filter(([, value]) => value !== true);
  console.log(JSON.stringify({ checks, failed: failed.length }, null, 2));
  if (failed.length) {
    console.error(`\n${failed.length} 项断言失败`);
    process.exit(1);
  }
  console.log(`\n全部 ${Object.keys(checks).length} 项断言通过`);
}
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
const byTag = (root, tag) => findAll(root, (n) => n.tagName === tag);
const click = (node) => { for (const fn of node.listeners.click || []) fn({ target: node, preventDefault() {} }); };

// —— 渲染真实页面 ——
const host = renderRouting({});
await new Promise((resolve) => setTimeout(resolve, 0));
await new Promise((resolve) => setTimeout(resolve, 0));

// —— 布局：模型列表必须竖排在左侧，横向标签页条必须消失 ——
const split = byClass(host, "rail-split")[0];
const rail = byClass(host, "rail-nav")[0];
check("split_present", Boolean(split));
check("rail_is_nav", rail?.tagName === "nav", rail?.tagName);
check("rail_is_first_column", split?.children[0] === rail);
check("detail_is_second_column", hasClass(split?.children[1] || new FakeNode("x"), "rail-detail"));
// 这条是本次改动的核心：模型路由页原来用横排标签页，必须确认它真的没了。
check("old_tabs_strip_removed", byClass(host, "tabs").length === 0);
check("no_fake_tab_semantics", byClass(host, "tab").length === 0);
check("detail_holds_card", byClass(split?.children[1] || new FakeNode("x"), "card").length === 1);

// —— 导航项：每条路由一行，名称与目标数都要在 ——
const items = byClass(rail || new FakeNode("x"), "rail-item");
check("rail_item_count", items.length === ROUTES.length, `${items.length} != ${ROUTES.length}`);
check("rail_marks_active", items[0]?.attrs["aria-current"] === "true", items[0]?.attrs["aria-current"]);
check("rail_others_not_current", items.slice(1).every((n) => n.attrs["aria-current"] === undefined));
check("rail_shows_route_id", items[0]?.textContent.includes("gpt-5.5") === true, items[0]?.textContent);
// 目标数是选模型时最该看到的读数（0 个目标的模型其实是坏的）。
check("rail_shows_target_count", items[0]?.textContent.includes("2 个目标") === true, items[0]?.textContent);
check("rail_shows_zero_targets", items[2]?.textContent.includes("0 个目标") === true, items[2]?.textContent);
check("rail_semantics_not_fake_tab", items.every((n) => n.attrs.role === undefined));
// 供应商那栏靠品牌图标区分，这里刻意不带图标：别把两类导航的差异悄悄抹平。
check("rail_has_no_duplicate_icons", byClass(rail || new FakeNode("x"), "icon").length === 0);

// 结构不对就不再往下驱动：后面的断言全部依赖 rail-item（点它才能切模型、开编辑器），
// 拿不到就没法验证，硬跑只会抛错。这里如实报出布局失败并停下。
if (!items.length) {
  report();
}

// —— 详情：整行文本必须能认出是哪条路由 ——
const detailText = split?.children[1]?.textContent || "";
check("detail_is_active_route", detailText.includes("gpt-5.5"), detailText.slice(0, 80));
check("detail_shows_aliases", detailText.includes("gpt-latest"));
check("detail_shows_hidden_aliases", detailText.includes("gpt-internal"));
// 目标按顺序列出：顺序即优先级，所以顺序本身是数据，不是排版细节。
check("detail_shows_targets_in_order",
  detailText.indexOf("openai / primary") < detailText.indexOf("openai / backup"), detailText.slice(0, 160));
check("detail_shows_routing_mode", detailText.includes("轮询"));

// —— 切换模型：详情必须跟着换，不能停在原来那条上 ——
click(items[2]);
const detail2 = byClass(host, "rail-detail")[0];
check("switch_updates_current", byClass(host, "rail-nav")[0]?.children[2]?.attrs["aria-current"] === "true");
check("switch_repaints_detail", detail2?.textContent.includes("local-llama") === true, detail2?.textContent?.slice(0, 80));
// 空目标要给出明确说明，而不是把"没有目标"渲染成一片空白。
check("switch_shows_empty_targets", detail2?.textContent.includes("尚未绑定目标 Key") === true);
check("switch_drops_old_detail", detail2?.textContent.includes("gpt-internal") !== true);

// —— 编辑态：展开后仍在右栏内，左栏的模型列表不能被整块替换掉 ——
const editButton = byTag(host, "button").find((n) => n.textContent.trim() === "编辑");
check("edit_button_present", Boolean(editButton));
click(editButton);
const detail3 = byClass(host, "rail-detail")[0];
check("editor_stays_in_detail_column", detail3?.textContent.includes("绑定目标 Key") === true,
  detail3?.textContent?.slice(0, 120));
check("rail_still_present_while_editing", byClass(host, "rail-nav")[0]?.children.length === ROUTES.length);
// 取消后回到只读详情。
const cancelButton = byTag(host, "button").find((n) => n.textContent.trim() === "取消");
check("editor_has_cancel", Boolean(cancelButton));
click(cancelButton);
check("cancel_returns_to_readonly",
  (byClass(host, "rail-detail")[0]?.textContent || "").includes("路由目标（按顺序）"));

// —— 候选目标去重：已绑定的 Key 不能再出现在候选里 ——
// 这条锁的是模板字符串里 `${target.key}` 曾被转义成字面量的回归：那样去重集合的键
// 与候选键永远不相等，已绑定的 Key 会被反复加进来。
//
// 必须先切回 gpt-5.5（上一步停在零目标的 local-llama 上），否则候选列表本就是空的，
// 断言会因为"没有候选"而假通过——那正是这个回归最难被发现的地方。
click(byClass(host, "rail-nav")[0].children[0]);
click(byTag(host, "button").find((n) => n.textContent.trim() === "编辑"));
const candidateButtons = byTag(host, "button").filter((n) => n.textContent.trim().startsWith("+ "));
check("candidates_rendered", candidateButtons.length > 0,
  candidateButtons.map((n) => n.textContent.trim()).join(" | "));
// openai/primary 已绑定，openai/secondary 未绑定：候选里只能有后者。
check("bound_keys_absent_from_candidates",
  candidateButtons.every((n) => !n.textContent.includes("primary")),
  candidateButtons.map((n) => n.textContent.trim()).join(" | "));
check("unbound_key_offered_as_candidate",
  candidateButtons.some((n) => n.textContent.includes("openai / secondary")),
  candidateButtons.map((n) => n.textContent.trim()).join(" | "));
// 探测不到该模型的 Key 不该出现在候选里（绑上去也调不通）。
check("candidates_only_cover_active_model",
  candidateButtons.every((n) => !n.textContent.includes("anthropic")),
  candidateButtons.map((n) => n.textContent.trim()).join(" | "));

report();
