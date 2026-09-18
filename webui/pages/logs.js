// 服务日志：把运行日志单独成一页。
//
// 从「用量统计」（原「实时活动」）里拆出来的理由：日志是**排查工具**，和用量数字
// 不是一回事。两者挤在一页时，日志的 2 秒轮询与整页重绘互相牵制 —— 用量看的是
// 分钟级以上的历史，却被迫跟着日志一起刷新；而日志需要的"说了什么"的检索与
// 级别过滤，混在数字看板中间也不好用。
//
// 数据来自 /api/logs：服务端只回最近 64 KiB，截断在服务端完成。

import { h, errorText } from "../dom.js";
import { api } from "../api.js";
import {
  card, cardHead, notice, badge, render, buttonNode, segmented, freshness, toggle, input,
} from "../ui.js";

// 刷新档位（毫秒）。2 秒用于盯实时，暂停用于安静地翻看历史。
const INTERVALS = [
  { id: 2000, label: "2 秒" },
  { id: 5000, label: "5 秒" },
  { id: 15000, label: "15 秒" },
  { id: 0, label: "暂停" },
];

// 级别筛选。判定口径与着色共用 levelOf()，避免"显示成红色却没被警告筛选中"。
const LEVELS = [
  { id: "all", label: "全部" },
  { id: "warn", label: "警告以上" },
  { id: "error", label: "仅错误" },
];

const state = {
  text: null,
  error: null,
  at: null,
  interval: 2000,
  level: "all",
  follow: true,
  query: "",
};

let host = null;
let timer = null;
let preNode = null;
let footNode = null;

export function renderLogs(context) {
  const { store } = context;
  host = h("div.stack");

  if (store.health?.local_auth_enabled && !store.authorized) {
    return h("div.stack", {}, notice("需要本地鉴权 Key 才能读取服务日志。", "warn"));
  }

  // 每次进入页面都重新起轮询并重新注册清理：上次离开时已经把它们清掉了。
  restartTimer();
  load();
  context.onLeave?.(stopTimer);
  draw(true);
  return host;
}

function restartTimer() {
  stopTimer();
  if (!state.interval) return;
  timer = setInterval(load, state.interval);
}

function stopTimer() {
  if (timer !== null) {
    clearInterval(timer);
    timer = null;
  }
}

async function load() {
  try {
    const data = await api.logs();
    if (data.error) {
      state.error = data.error;
      state.text = null;
    } else {
      state.error = null;
      state.text = data.text || "";
    }
    state.at = new Date().toISOString();
  } catch (error) {
    state.error = errorText(error);
  }
  paint();
}

// levelOf 从行里识别日志级别。
//
// 服务端给的是**已渲染的文本**（不是结构化记录），级别只以单词形式存在于行内，
// 所以这里按词匹配而不是解析格式。识别不出级别的行归为 "none"：它们仍会显示，
// 只是在"警告以上/仅错误"筛选下被排除。
function levelOf(line) {
  const match = /\b(trace|debug|info|warn(?:ing)?|error|fatal|critical)\b/i.exec(line);
  if (!match) return { level: "none", tone: "" };
  const word = match[1].toLowerCase();
  if (/error|fatal|critical/.test(word)) return { level: "error", tone: "lv-error" };
  if (word.startsWith("warn")) return { level: "warning", tone: "lv-warning" };
  if (word === "info") return { level: "info", tone: "lv-info" };
  return { level: "debug", tone: "lv-debug" };
}

function allLines() {
  return (state.text || "").split(/\r?\n/).filter((line) => line.trim() !== "");
}

function visibleLines() {
  const query = state.query.trim().toLowerCase();
  return allLines().filter((line) => {
    const { level } = levelOf(line);
    if (state.level === "error" && level !== "error") return false;
    if (state.level === "warn" && level !== "error" && level !== "warning") return false;
    if (query && !line.toLowerCase().includes(query)) return false;
    return true;
  });
}

function draw(firstPaint = false) {
  if (!host) return;
  if (!firstPaint && !host.isConnected) return;

  const total = allLines().length;
  const shown = visibleLines().length;

  const children = [
    h("div.page-head", {},
      h("div.page-title", {},
        h("h1", "服务日志"),
        h("p.sub", "最近 64 KiB 运行日志，用于排查请求失败与上游异常。"),
      ),
      h("div.page-actions", {},
        state.at ? freshness(state.at, { prefix: "拉取于" }) : null,
        buttonNode("立即刷新", {
          variant: "secondary", small: true, iconName: "refresh",
          onClick: () => load(),
        }),
      ),
    ),
  ];

  children.push(card(
    cardHead("日志",
      badge(state.interval ? `${state.interval / 1000} 秒刷新` : "已暂停", state.interval ? "muted" : "warn"),
      badge("最近 64 KiB", "muted"),
      h("div.head-tools", {},
        segmented(INTERVALS.map((item) => ({ id: item.id, label: item.label })),
          state.interval,
          (id) => { state.interval = Number(id); restartTimer(); draw(); },
          { "aria-label": "选择刷新间隔" }),
      ),
    ),
    // 筛选条：级别 + 关键字 + 自动跟随。跟随开关放在这里而不是页头，
    // 因为它只影响这一个面板。
    h("div.toolbar", {},
      segmented(LEVELS.map((item) => ({ id: item.id, label: item.label })),
        state.level,
        (id) => { state.level = id; draw(); },
        { "aria-label": "按级别过滤日志" }),
      input({
        type: "search",
        value: state.query,
        placeholder: "搜索关键字…",
        "aria-label": "在日志中搜索",
        style: { maxWidth: "260px" },
        // 输入时不重绘整个面板（会丢焦点），只重画 <pre> 与脚注。
        onInput: (event) => { state.query = event.target.value; paint(); updateFoot(); },
      }),
      h("span.spacer"),
      toggle("自动跟随最新", state.follow, () => {
        state.follow = !state.follow;
        draw();
        // 打开跟随时立刻贴到底部，否则要等下一行日志才生效。
        if (state.follow && preNode) preNode.scrollTop = preNode.scrollHeight;
      }),
    ),
    logPanel(),
    footPanel(total, shown),
  ));

  render(host, children);
  // render() 会清空 host，上面那几个节点就是刚挂上去的实例，直接持有引用即可 ——
  // 不用 querySelector 回查：那样一旦类名改了、或节点嵌套变了，查找会静默返回
  // null（局部刷新就悄悄不生效了），而持有引用在写的时候就暴露问题。
  applyFollow();
  paint();
}

function logPanel() {
  const pre = h("pre.log-panel", { tabindex: "0" }, "正在读取服务日志…");
  preNode = pre;
  return pre;
}

function footPanel(total, shown) {
  footNode = h("div.card-foot", {}, footText(total, shown));
  return footNode;
}

function footText(total, shown) {
  if (state.error) return "日志不可用。";
  if (!state.text) return "日志为空。";
  if (shown === total) return `共 ${total} 行。`;
  return `显示 ${shown} / ${total} 行（已按筛选条件过滤）。`;
}

function updateFoot() {
  // 用 render（= replaceChildren）而不是给 textContent 赋值：与全项目一致，
  // 也避免依赖"textContent 可写"这一条（DOM 垫片里它是只读的）。
  if (footNode) render(footNode, footText(allLines().length, visibleLines().length));
}

function applyFollow() {
  if (state.follow && preNode) preNode.scrollTop = preNode.scrollHeight;
}

function paint() {
  const pre = preNode;
  if (!pre || !pre.isConnected) return;
  // 只有用户本来就贴在底部时才自动跟滚，否则会打断向上翻阅。
  const pinned = state.follow || pre.scrollHeight - pre.scrollTop - pre.clientHeight <= 8;

  if (state.error) {
    pre.replaceChildren(h("div.lv-error", `日志暂不可用: ${state.error}`));
    return;
  }
  const lines = visibleLines();
  if (!lines.length) {
    pre.replaceChildren(h("div", state.text ? "当前筛选条件下没有日志行。" : "日志为空。"));
    return;
  }
  pre.replaceChildren(...lines.map(lineNode));
  if (pinned) pre.scrollTop = pre.scrollHeight;
}

function lineNode(line) {
  const { tone } = levelOf(line);
  return h(tone ? `div.${tone}` : "div", line);
}
