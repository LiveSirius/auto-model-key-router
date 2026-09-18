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

const { lineChart, stackedBars, heatmap } = await import(pathToFileURL(path.join(WEBUI, "charts.js")).href);

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

// —— 缺口桥接 ——
// 回归：均值型指标在无请求的桶上没有读数（null），若只按"连续非空"画实线，
// 稀疏流量下一条曲线会碎成几十段 + 一地孤立圆点（真实 1 小时窗口出现过 26 段）。
// 这里锁定：缺口两侧的实测点必须被一条虚线连起来，孤立点不再单独散落。
const gapPoints = [
  { started_at: at(0), requests: 2, total_duration_ms: 20000, complete: true },
  { started_at: at(1), requests: 0, total_duration_ms: 0, complete: true },
  { started_at: at(2), requests: 0, total_duration_ms: 0, complete: true },
  { started_at: at(3), requests: 4, total_duration_ms: 40000, complete: true },
];
const gapHost = lineChart({ points: gapPoints, metricId: "latency", bucketSeconds: 60, height: 140, showArea: false });
const gapSvg = findAll(gapHost, (n) => n.tagName === "svg")[0];
const gapLines = findAll(gapSvg, (n) => hasClass(n, "series-gap"));
check("gap_bridge_rendered", gapLines.length === 1, String(gapLines.length));
// 桥必须真的跨越缺口：两端分别落在 index 0 与 index 3 的 x 上。
// 几何：宽度 720、left 留白 60、right 留白 16 ⇒ 内宽 644，共 4 点 ⇒ 步长 644/3。
if (gapLines.length === 1) {
  const coords = gapLines[0].attrs.points.trim().split(/\s+/)
    .map((pair) => Number(pair.split(",")[0]));
  const step = (720 - 60 - 16) / 3;
  check("gap_bridge_spans_gap",
    coords.length === 2 && coords[0] === 60 && Math.abs(coords[1] - (60 + 3 * step)) < 0.01,
    JSON.stringify(coords));
}
// 缺口两侧的实测点仍要各画各的：单点段画成圆点（这是原有的孤立点表示），
// 虚线只是补在中间，不会把它替换掉。
check("gap_keeps_measured_markers",
  findAll(gapSvg, (n) => n.tagName === "circle" && n.attrs.class === "point").length === 2,
  String(findAll(gapSvg, (n) => n.tagName === "circle" && n.attrs.class === "point").length));
// 0 是真实读数：RPM 的空闲段必须仍是实线，不能被误当作缺口桥掉。
const zeroHost = lineChart({
  points: [0, 1, 2, 3].map((i) => ({ started_at: at(i), requests: 0, complete: true })),
  metricId: "rpm", bucketSeconds: 60, height: 140, showArea: false,
});
const zeroSvg = findAll(zeroHost, (n) => n.tagName === "svg")[0];
check("gap_zero_rpm_has_no_bridge", findAll(zeroSvg, (n) => hasClass(n, "series-gap")).length === 0);
check("gap_zero_rpm_is_solid_line", findAll(zeroSvg, (n) => hasClass(n, "series-line")).length === 1);

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

// —— 热力图 ——
// 336 个格子必须与 (星期, 半小时) 一一对应，且"窗口未覆盖"要与"这一格是 0"
// 区分开 —— 两者画成同一格就等于谎报。这里同时锁住落格顺序与格子档位。
const heatMetric = {
  id: "requests",
  label: "请求数量",
  pick: (point) => Number(point.requests) || 0,
  format: (value) => `${value} 次`,
};
const heatHost = heatmap({
  points: [
    { started_at: "2026-01-01T10:00:00+08:00", requests: 15, complete: false },
    { started_at: "2026-01-04T23:00:00+08:00", requests: 100, complete: true },
  ],
  metric: heatMetric,
});
// 只取矩阵里的格子：图例里也有 .heat-cell，按 DOM 顺序落在 .heatmap 之后。
const heatGrid = findAll(heatHost, (n) => hasClass(n, "heatmap"))[0];
const gridCells = heatGrid ? findAll(heatGrid, (n) => hasClass(n, "heat-cell")) : [];
const heatTip = findAll(heatHost, (n) => hasClass(n, "chart-tip"))[0];
check("heat_grid_present", Boolean(heatGrid));
check("heat_cell_count", gridCells.length === 336, String(gridCells.length));
check("heat_tip_present", Boolean(heatTip));

if (gridCells.length === 336 && heatTip) {
  // 2026-01-01 是周四 ⇒ weekday 3；10:00 ⇒ 半小时索引 20（3*48+20）。
  const thursday10 = gridCells[3 * 48 + 20];
  // 23:00 ⇒ 半小时索引 46（6*48+46）。
  const sunday23 = gridCells[6 * 48 + 46];
  const coverless = gridCells[0];
  check("heat_thursday10_level1", hasClass(thursday10, "level-1"), thursday10.className);
  check("heat_sunday23_is_max_level", hasClass(sunday23, "level-4"), sunday23.className);
  check("heat_coverless_marked", hasClass(coverless, "is-coverless"), coverless.className);
  check("heat_partial_marked", hasClass(thursday10, "is-partial"), thursday10.className);
  check("heat_complete_not_partial", !hasClass(sunday23, "is-partial"), sunday23.className);

  thursday10.listeners.pointerenter[0]();
  check("heat_tip_shows_slot", heatTip.textContent.includes("周四 10:00"), heatTip.textContent);
  check("heat_tip_shows_value", heatTip.textContent.includes("15 次"), heatTip.textContent);
  check("heat_tip_marks_partial", heatTip.textContent.includes("累加中"), heatTip.textContent);
  check("heat_tip_has_no_null_row", noStrayNull(heatTip), heatTip.textContent);

  coverless.listeners.pointerenter[0]();
  check("heat_tip_coverless_says_so", heatTip.textContent.includes("窗口未覆盖"), heatTip.textContent);
  check("heat_tip_coverless_no_zero", !heatTip.textContent.includes("0 次"), heatTip.textContent);
}

const failed = Object.entries(checks).filter(([, value]) => value !== true);
console.log(JSON.stringify({
  failed: failed.map(([name, detail]) => `${name} (${detail})`),
  total: Object.keys(checks).length,
}));
process.exit(failed.length ? 1 : 0);
