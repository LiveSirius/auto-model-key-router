// 图表首帧宽度回归探针：锁住"图表往中间挤一下又复原"这个 bug。
// 用法：node webui_chart_sizing_probe.mjs，结果以 JSON 打到 stdout，失败时退出码 1。
//
// 为什么必须单独有这么一个探针：现有的 tip 探针刻意把 ResizeObserver 设为 undefined，
// 走的是"测一次就画"的降级分支，永远碰不到这里的问题；chart 探针又只测 chart-math.js 的
// 纯函数、根本不建 DOM。
//
// 这个 bug 的完整机制（已在真实 Chrome 里逐帧复核过）：
//   1. 页面都是先 h() 建好图表节点、之后才 mount 进文档（见 overview/activity/cost 的 draw）；
//   2. 建节点时 sizing() 立刻画了首帧，而脱文档的节点量不到宽度，
//      于是退回 720 的兜底宽度 → 画出一张 width="720" 的 <svg>；
//   3. mount 之后 CSS `.chart { width: 100% }` 把 <svg> 元素拉满容器（比如 1200px），
//      但 viewBox 仍是 "0 0 720 …"，preserveAspectRatio 默认居中等比缩放，
//      整幅图就被缩到中间（网格从 x=60..1184 缩成 300..944）；
//   4. 紧接着 ResizeObserver 回调按真实宽度重画，图"啪"地弹回满宽。
//   每次轮询重绘（10 秒一次）都会重演一遍，肉眼看到的就是内缩动画。
//
// 所以垫片必须忠实还原三件事，否则整条回归链根本不会触发：
//   * 脱文档节点的 clientWidth 必须是 0（不能像 tip 探针那样恒返回 720）；
//   * 从 DOM 里移除子节点时必须清掉它的 parent（否则 isConnected 恒真，
//     sizing() 里"脱离文档就断开观察者"的分支永远走不到）；
//   * ResizeObserver 只在**尺寸真的变了**时异步回调（构造时同步回调、
//     或每次 flush 都无条件回调，都会把真实时序抹平）。

import { pathToFileURL } from "node:url";
import path from "node:path";

const WEBUI = path.resolve(import.meta.dirname, "..");

// —— 绘制记录 ——
// 图表画图的唯一出口是 host.replaceChildren(...)，所以在垫片内部全局记录，
// 再按节点身份归属到具体图例。不能等 lineChart() 返回后再包一层：
// 选择 ResizeObserver 分支时首绘发生在构造过程中，那时还没法包装。
const paints = [];
const labels = new Map();
const label = (node, name) => { labels.set(node, name); return node; };
const findFirst = (node, predicate) => {
  if (predicate(node)) return node;
  for (const child of node.children) {
    const hit = findFirst(child, predicate);
    if (hit) return hit;
  }
  return null;
};
const recordPaint = (node) => {
  const svgNode = findFirst(node, (n) => n.tagName === "svg");
  paints.push({
    node,
    connected: node.isConnected,
    // 记下那一刻容器量到的宽度，便于判断"画出来的宽度是不是当时该用的宽度"。
    containerWidth: node.isConnected ? node.clientWidth : null,
    painted: svgNode ? svgNode.getAttribute("width") : null,
    viewBox: svgNode ? svgNode.getAttribute("viewBox") : null,
  });
};

class FakeNode {
  constructor(tag) {
    this.tagName = tag;
    this.children = [];
    this.attrs = {};
    this.listeners = {};
    this.className = "";
    this.style = {};
    this.parent = null;
    // 认为自己是布局容器时的宽度；挂载后才量得到。
    this.width = 0;
  }
  append(...nodes) { this.#adopt(nodes); }
  replaceChildren(...nodes) {
    // 真实 DOM 会把被移除的子节点彻底摘下来（parent 置空）。
    for (const child of this.children) child.parent = null;
    this.children = [];
    this.#adopt(nodes);
    recordPaint(this);
  }
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
    this.parent = null;
  }
  querySelector() { return null; }
  focus() {}
  get parentElement() { return this.parent; }
  // 关键：脱文档（或 display:none）的节点在浏览器里量不到宽度，必须还原成 0。
  // 恒返回 720 会让这个 bug 永远测不出来。
  //
  // 自身没设宽度时随父容器走，模型化 styles.css 里的 `.chart-host { width: 100% }`：
  // 图表容器自身没有固定宽度，宽度完全继承自卡片。
  get clientWidth() {
    if (!this.isConnected) return 0;
    return this.width || (this.parent ? this.parent.clientWidth : 0);
  }
  get offsetWidth() { return this.clientWidth; }
  getBoundingClientRect() {
    return { left: 0, top: 0, width: this.clientWidth, height: 280 };
  }
  get isConnected() {
    let node = this;
    while (node) {
      if (node === documentRoot) return true;
      node = node.parent;
    }
    return false;
  }
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

class FakeText extends FakeNode {
  constructor(data) { super("#text"); this.data = String(data); }
  get textContent() { return this.data; }
}

const documentRoot = new FakeNode("#document");
const body = new FakeNode("body");
documentRoot.append(body);

global.Node = FakeNode;
global.document = {
  createElement: (tag) => new FakeNode(tag),
  createElementNS: (_ns, tag) => new FakeNode(tag),
  createTextNode: (text) => new FakeText(text),
  body,
};

// —— 异步、且只在尺寸变化时回调的 ResizeObserver 垫片 ——
// 真实 RO 不会在 observe() 里同步回调，而是在布局之后异步送一次初始尺寸；
// 之后也只在**尺寸真的变了**时再回调（这条正是 RO 不会自我循环的原因）。
// 元素脱离文档时尺寸变成 0，因此会再送一次回调 —— sizing() 依赖这次回调断开观察者。
const observers = [];
class FakeResizeObserver {
  constructor(callback) {
    this.callback = callback;
    this.targets = [];
    this.sizes = new Map();
    this.active = true;
    observers.push(this);
  }
  observe(target) {
    this.targets.push(target);
    this.sizes.delete(target); // 初始投递必定发生
  }
  unobserve(target) { this.targets = this.targets.filter((t) => t !== target); }
  disconnect() { this.active = false; this.targets = []; }
}
global.ResizeObserver = FakeResizeObserver;

// 目标当前被测到的尺寸：脱文档时报 0，与浏览器一致。
const measuredSize = (target) => target.clientWidth;
// 只投递尺寸真变了的那些目标。
const flushObservers = () => {
  for (const observer of observers) {
    if (!observer.active) continue;
    const changed = observer.targets.filter((target) => {
      const size = measuredSize(target);
      if (observer.sizes.get(target) === size) return false;
      observer.sizes.set(target, size);
      return true;
    });
    if (changed.length) observer.callback(changed.map((target) => ({ target })), observer);
  }
};

// requestAnimationFrame 同理：只排队，等探针 flush（当前实现已不排队，留作护栏）。
let rafSeq = 0;
const rafQueue = new Map();
global.requestAnimationFrame = (fn) => { const id = (rafSeq += 1); rafQueue.set(id, fn); return id; };
global.cancelAnimationFrame = (id) => { rafQueue.delete(id); };
const flushFrames = () => {
  const queued = [...rafQueue.values()];
  rafQueue.clear();
  for (const fn of queued) fn(0);
};
// 走完一轮"RO 回调 → 下一帧"的完整流程（顺序与浏览器一致）。
const settle = () => { flushObservers(); flushFrames(); flushObservers(); flushFrames(); };

const { lineChart, stackedBars } = await import(pathToFileURL(path.join(WEBUI, "charts.js")).href);

const checks = {};
const check = (name, ok, detail = "") => {
  checks[name] = ok ? true : `FAILED${detail ? `: ${detail}` : ""}`;
};

const at = (minute) => `2026-01-01T10:${String(minute).padStart(2, "0")}:00+08:00`;
const points = [0, 1, 2, 3, 4].map((i) => ({
  started_at: at(i), requests: 10 + i, prompt_tokens: 100, completion_tokens: 50, complete: true,
}));

// 绘制时"应当"使用的宽度：与 sizing() 的取值口径一致（挂载后取自身宽度）。
const paintsOf = (name) => paints.filter((p) => labels.get(p.node) === name);
const lastPaint = (name) => paintsOf(name).at(-1);
// 打印用：剥掉 node 引用（它带 parent 回指，JSON.stringify 会因循环引用抛错）。
const show = (items) => JSON.stringify(items.map(({ node, ...rest }) => rest));

// —— 场景 1：页面真实时序 ——先建节点（未挂载），再 mount ——
const container = new FakeNode("div");
container.width = 1200;
body.append(container);

const line = label(lineChart({ points, metricId: "rpm", bucketSeconds: 60, height: 280 }), "line");
const bars = label(stackedBars({ points, bucketSeconds: 60, height: 220 }), "bars");

// 核心断言：量不到宽度时一次都不许画。
// 这一条直接对应 bug 的根因——那时画出来的只可能是 720 兜底宽度，挂载后必被拉伸内缩。
check("no_paint_while_detached_line", paintsOf("line").length === 0, show(paintsOf("line")));
check("no_paint_while_detached_bars", paintsOf("bars").length === 0, show(paintsOf("bars")));

// mount + 一轮布局回调
container.replaceChildren(line, bars);
settle();

check("line_painted_once_after_mount", paintsOf("line").length === 1, show(paintsOf("line")));
check("line_painted_full_width", lastPaint("line")?.painted === "1200", show([lastPaint("line")]));
check("bars_painted_full_width", lastPaint("bars")?.painted === "1200", show([lastPaint("bars")]));
// viewBox 必须与 CSS 盒子同宽，否则 preserveAspectRatio 会把图形居中等比缩放。
check("line_viewbox_matches_width", lastPaint("line")?.viewBox === "0 0 1200 280", String(lastPaint("line")?.viewBox));
check("bars_viewbox_matches_width", lastPaint("bars")?.viewBox === "0 0 1200 220", String(lastPaint("bars")?.viewBox));
// 每一次绘制都必须发生在一个已挂载、且宽度正确的节点上。
// 这正是 bug 的反面：修复前会出现 connected=true 却画成 720 的那一帧。
check("every_paint_is_connected",
  paintsOf("line").concat(paintsOf("bars")).every((p) => p.connected),
  show(paintsOf("line").concat(paintsOf("bars")).filter((p) => !p.connected)));
check("no_paint_at_fallback_width_while_connected",
  paintsOf("line").concat(paintsOf("bars")).every((p) => !(p.connected && p.painted === "720")),
  show(paintsOf("line").concat(paintsOf("bars")).filter((p) => p.connected && p.painted === "720")));

// —— 场景 2：容器变宽/变窄仍要跟随重画 ——
const beforeResize = paintsOf("line").length;
container.width = 640;
settle();
check("line_tracks_shrink", lastPaint("line")?.painted === "640", show([lastPaint("line")]));
container.width = 980;
settle();
check("line_tracks_grow", lastPaint("line")?.painted === "980", show([lastPaint("line")]));
check("resize_triggered_repaint", paintsOf("line").length > beforeResize, String(paintsOf("line").length));

// —— 场景 3：图表被整块替换（页面每次轮询都会做）时不许泄漏观察者 ——
const liveBefore = observers.filter((o) => o.active).length;
container.replaceChildren(); // 节点脱离文档，RO 会收到一次尺寸归零的回调
settle();
const stillActive = observers.filter((o) => o.active).length;
check("detached_chart_disconnects", stillActive === liveBefore - 2, `active ${liveBefore} -> ${stillActive}`);
// 断开之后容器再变化，不许再有任何绘制。
const paintsAfterDetach = paints.length;
container.width = 1500;
settle();
check("no_paint_after_detach", paints.length === paintsAfterDetach,
  show(paints.slice(paintsAfterDetach)));

// —— 场景 4：没有 ResizeObserver 的降级路径必须照旧立即绘制 ——
// tip 探针就跑在这条分支上，改坏了它整个探针会静默测不到东西。
const savedRO = global.ResizeObserver;
global.ResizeObserver = undefined;
label(lineChart({ points, metricId: "rpm", bucketSeconds: 60, height: 280 }), "fallback");
check("fallback_paints_immediately", paintsOf("fallback").length === 1, show(paintsOf("fallback")));
// 脱文档时按历史兜底宽度 720 画（与 tip 探针垫片的 clientWidth 一致）。
check("fallback_uses_measured_width", lastPaint("fallback")?.painted === "720", show([lastPaint("fallback")]));
global.ResizeObserver = savedRO;

const failed = Object.entries(checks).filter(([, value]) => value !== true);
console.log(JSON.stringify({
  failed: failed.map(([name, detail]) => `${name} (${detail})`),
  total: Object.keys(checks).length,
}));
process.exit(failed.length ? 1 : 0);
