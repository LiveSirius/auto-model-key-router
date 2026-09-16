// Tooltip 读数回归探针：用最小 DOM 垫片驱动真实的 webui/charts.js。
// 用法：node webui_tip_probe.mjs，结果以 JSON 打到 stdout，失败时退出码 1。
//
// 为什么必须用 DOM 垫片：图表工具提示读的是真实指针事件渲染出来的文本，
// chart-math.js 的纯函数测不到它。而这个垫片必须忠实还原浏览器语义 ——
// 原生 replaceChildren 会把 null 参数字符串化成一个文本节点 "null"，
// 曾经因此让每个已完结的点在气泡里多出一行 null。垫片若在这里"好心"过滤
// 空值，就会把这个 bug 连同它的回归保护一起藏掉。

import { pathToFileURL } from "node:url";
import path from "node:path";

const WEBUI = path.resolve(import.meta.dirname, "../auto_model_key_router/webui");

class FakeNode {
  constructor(tag) {
    this.tagName = tag;
    this.children = [];
    this.attrs = {};
    this.listeners = {};
    this.className = "";
    this.style = {};
    this.parent = null;
  }
  // 与 webui/dom.js 的 append() 对齐：它自己会过滤 null，再交给这里。
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
  focus() {}
  get clientWidth() { return 720; }
  getBoundingClientRect() { return { left: 0, top: 0, width: 720, height: 280 }; }
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

const root = new FakeNode("div");
global.Node = FakeNode;
global.document = {
  createElement: (tag) => new FakeNode(tag),
  createElementNS: (_ns, tag) => new FakeNode(tag),
  createTextNode: (text) => new FakeText(text),
  body: new FakeNode("body"),
};
// 探针环境无 ResizeObserver：charts.js 会按测量宽度直接画一次，正是我们要的。
global.ResizeObserver = undefined;

const { lineChart, stackedBars } = await import(pathToFileURL(path.join(WEBUI, "charts.js")).href);

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
// 气泡里除了应有的读数，不允许再多出任何一行文本。
const noStrayNull = (node) => !node.children.some((c) => c.textContent === "null");

const at = (minute) => `2026-01-01T10:${String(minute).padStart(2, "0")}:00+08:00`;
const points = [0, 1, 2, 3, 4].map((i) => ({
  started_at: at(i), requests: 10 + i, complete: true,
}));
points[4].complete = false; // 尾桶累加中

// —— 折线气泡 ——
// 宽度 720、左侧留白 60、共 5 点 ⇒ 步长 161px，index 2 落在 x=382。
const lineHost = lineChart({ points, metricId: "rpm", bucketSeconds: 60, height: 280 });
const lineTip = findAll(lineHost, (n) => hasClass(n, "chart-tip"))[0];
const overlay = findAll(lineHost, (n) => n.tagName === "rect" && n.listeners.pointermove?.length)[0];
check("line_overlay_present", Boolean(overlay));
check("line_tip_present", Boolean(lineTip));

if (overlay && lineTip) {
  const move = (index) => overlay.listeners.pointermove[0]({ clientX: 60 + index * 161 });

  move(2);
  check("line_tip_shows_value", lineTip.textContent.includes("12 次/分"), lineTip.textContent);
  check("line_tip_shows_time", lineTip.textContent.includes("10:02"), lineTip.textContent);
  check("line_tip_has_no_null_row", noStrayNull(lineTip), lineTip.textContent);
  check("line_tip_complete_has_no_flag", !lineTip.textContent.includes("累加中"), lineTip.textContent);

  move(4);
  check("line_tip_marks_partial", lineTip.textContent.includes("累加中"), lineTip.textContent);
  check("line_tip_partial_has_no_null_row", noStrayNull(lineTip), lineTip.textContent);
}

// —— 堆叠柱气泡 ——
const barPoints = [0, 1, 2].map((i) => ({
  started_at: at(i), prompt_tokens: 100 + i, completion_tokens: 40, complete: true,
}));
barPoints[2].complete = false;
const barHost = stackedBars({ points: barPoints, height: 200 });
const barTip = findAll(barHost, (n) => hasClass(n, "chart-tip"))[0];
const bars = findAll(barHost, (n) => hasClass(n, "bar"));
check("bars_present", bars.length === barPoints.length * 2, String(bars.length));
check("bar_tip_present", Boolean(barTip));

if (barTip && bars.length) {
  bars[0].listeners.pointerenter[0]();
  check("bar_tip_shows_totals", barTip.textContent.includes("合计 140"), barTip.textContent);
  check("bar_tip_has_no_null_row", noStrayNull(barTip), barTip.textContent);
  check("bar_tip_complete_has_no_flag", !barTip.textContent.includes("累加中"), barTip.textContent);

  // 每点两根柱（输入/输出），第 3 点的柱从 index 4 起，是那个累加中的尾桶。
  bars[4].listeners.pointerenter[0]();
  check("bar_tip_marks_partial", barTip.textContent.includes("累加中"), barTip.textContent);
  check("bar_tip_partial_has_no_null_row", noStrayNull(barTip), barTip.textContent);
}

const failed = Object.entries(checks).filter(([, value]) => value !== true);
console.log(JSON.stringify({
  failed: failed.map(([name, detail]) => `${name} (${detail})`),
  total: Object.keys(checks).length,
}));
process.exit(failed.length ? 1 : 0);
