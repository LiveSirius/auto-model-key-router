// 访客看板：访问密钥持有者看**自己**的用量。
//
// 「访客」不是一个新的身份体系——它就是访问密钥（amkr_ak_…）的持有者。原先的访客
// 模式是一把全局共享的固定 key，因此谈不上"自己的用量"；访问密钥一人一把之后，
// 「这个人用了多少、用了哪些模型」才成为一个有意义的问题。
//
// 三条边界（都由服务端强制，前端只是不提供入口）：
//
//   1. **只读**。这一页只调 /ui/access-key-usage.json，任何写操作都不属于访客；
//      清单的修改在管理面的「访问密钥」页。
//   2. **范围完全由 key 决定**。不发 workspace、不发 key_id：能看见多少就是这把 key
//      自己的流量。多发一个参数只会让人以为换个值就能看别人的。
//   3. **不回显明文 key**。页头只显示 key 的名字（服务端从配置里取）与指纹式的
//      尾部片段，避免投屏/截图时把凭据一起交出去。
//
// 与工作空间面板（webui/panel.js）的分工：那个是嵌入方的**空间**视图、能管任务；
// 这个是访客的**密钥**视图、纯只读。两者共用同一套组件与样式，但入口与权限面不同。

import {
  h, formatCount, formatCompact, formatDateTime, formatDuration, errorText, truncate,
} from "../dom.js";
import { ApiError } from "../api.js";
import {
  createGuestApi, guestCredential, saveGuestCredential,
} from "../guest-api.js";
import {
  installToastHost, toast, card, cardHead, stat, statGrid, notice, badge, empty,
  skeleton, render, segmented, table, freshness, buttonNode, field, input,
} from "../ui.js";
import { barList } from "../charts.js";
import {
  USAGE_RANGES, ALL_HISTORY, rangeSpec, formatPercentValue,
} from "../chart-math.js";
import {
  loadPricing, pricingStatus, currentIndex, sumCost, formatCost, formatCostExact,
} from "../pricing.js";

const state = {
  selection: 24,
  key: "",
  usage: null,
  // unauthorized 与 error 分开：前者是"凭据不对"，界面该请人重新给 key；后者是
  // "请求出错了"，界面该显示错误并让人重试。混成一个会让 401 看起来像服务故障。
  unauthorized: false,
  // disabled 单独一档：服务端对停用的 key 回 403。它不是"key 打错了"，而是"这把
  // key 被管理员关了"——文案与可采取的动作都不同。
  disabled: false,
  error: null,
  loading: true,
  pricingError: null,
  pricingLoading: false,
  at: null,
};

let api = null;
let host = null;

export function bootGuest() {
  const root = document.getElementById("root");
  host = h("div.panel");
  root.append(host);
  installToastHost(root);
  // 没有 key 就先问人要，而不是直接打接口——否则用户看到的是一个 401 错误，
  // 会以为 key 填错了（其实还没填）。
  state.key = guestCredential();
  if (!state.key) {
    state.loading = false;
    draw();
    return;
  }
  api = createGuestApi(state.key);
  load();
  loadCatalog();
}

// —— 数据 ——

function spec() {
  return rangeSpec(state.selection);
}

async function load() {
  if (!api) return;
  const current = spec();
  try {
    state.usage = await api.usage({
      hours: current.hours,
      allHistory: state.selection === ALL_HISTORY,
    });
    state.unauthorized = false;
    state.disabled = false;
    state.error = null;
    state.at = new Date().toISOString();
  } catch (error) {
    state.usage = null;
    if (error instanceof ApiError && error.status === 401) {
      state.unauthorized = true;
      state.error = null;
    } else if (error instanceof ApiError && error.status === 403) {
      state.disabled = true;
      state.error = null;
    } else {
      state.error = errorText(error);
    }
  }
  state.loading = false;
  draw();
}

// loadCatalog 取价格目录（附加读数，失败不影响主读数）。
//
// 目录不鉴权，因此即使 key 无效也能取到；失败时成本列显示 "—"，绝不显示 $0
// （见 webui/pricing.js 的说明：0 会被读成"这次请求免费"）。
async function loadCatalog() {
  if (!host) return;
  state.pricingLoading = true;
  try {
    await loadPricing(() => api.pricing());
    state.pricingError = pricingStatus().error;
  } catch (error) {
    state.pricingError = errorText(error);
  }
  state.pricingLoading = false;
  draw();
}

// —— 凭据表单 ——

function keyForm() {
  const box = input({
    type: "password",
    placeholder: "amkr_ak_…",
    autocomplete: "off",
    "aria-label": "访问密钥",
  });
  const submit = () => {
    const value = box.value.trim();
    if (!value) {
      toast("请填写访问密钥。", "error");
      return;
    }
    saveGuestCredential(value);
    api = createGuestApi(value);
    state.key = value;
    state.loading = true;
    state.unauthorized = false;
    state.disabled = false;
    state.error = null;
    draw();
    load();
    loadCatalog();
  };
  box.addEventListener("keydown", (event) => { if (event.key === "Enter") submit(); });
  return card(
    cardHead("访客看板"),
    h("p.muted", "用你的访问密钥登录，查看这把密钥自己的用量。密钥由 AMKR 管理员"
      + "在「访问密钥」页创建并分发给你；它决定了你能调用哪些供应商与模型。"),
    field("访问密钥", box),
    h("div.inline", {}, buttonNode("查看看板", { variant: "primary", onClick: submit })),
  );
}

// —— 派生读数 ——

function statsOf(usage) {
  return usage?.stats || null;
}

// rangeLabel 给出窗口的可读描述。
//
// 「全部历史」的真实跨度只有服务端知道（响应里的 window.from），因此不能像用量统计
// 页那样先探最早一条——访客读不了 /metrics/requests（那是完整权限的接口）。
function rangeLabel() {
  if (state.selection !== ALL_HISTORY) return spec().label;
  const from = state.usage?.window?.from;
  return from ? `全部历史（自 ${formatDateTime(from)}）` : "全部历史";
}

// dimensionEntries 把一个维度字典转成排行榜行。
//
// 后端给的是 {model_id: {model-a: stats, ...}, ...}，键名就是原始列名（与
// workspace.go 的 layers 同一约定），中文名只在这一处映射。
const DIMENSION_LABELS = {
  model_id: "模型",
  provider_id: "供应商",
  upstream_model_id: "上游模型",
};

function dimensionEntries(dimension, valueOf) {
  const grouped = state.usage?.dimensions?.[dimension] || {};
  return Object.entries(grouped)
    .map(([name, stats]) => ({ name, stats, value: valueOf(stats) }))
    .filter((row) => row.value > 0)
    .sort((a, b) => b.value - a.value);
}

// totalCost 估算本窗口的花费。
//
// 按 upstream_model_id 分组求和：那是唯一能与 models.dev 目录对上的字段（model_id
// 是调用方自取的本地路由名）。sumCost 会一并给出「几项里有价」，界面上必须显示，
// 否则合计会被误读成全量——一份只覆盖一半条目的金额看起来和完整的一样。
function totalCost() {
  const grouped = state.usage?.dimensions?.upstream_model_id;
  if (!grouped) return null;
  if (!currentIndex()) return null;
  return sumCost(currentIndex(), grouped);
}

// —— 组件 ——

// kpiTiles 渲染顶部四张瓦片。
//
// **固定四张，不随数据增减**：.stat-grid 在四档断点上是 4 / 2 / 1 列，瓦片数必须能被
// 每一档整除，否则窄屏会甩出孤零零的一张（webui/probes/webui_layout_probe.mjs 专门
// 守着这条）。花费因此不在这里凑第五张，而是在下面的「花费估算」卡里给合计——
// 那里也正是它需要解释口径（估算、几项有价）的地方。
function kpiTiles(usage) {
  const stats = statsOf(usage);
  const requests = stats?.requests || 0;
  const successes = stats?.successes || 0;
  return statGrid(
    stat("请求", formatCount(requests), rangeLabel(),
      { iconName: "activity", trendPolarity: "neutral" }),
    stat("成功率", requests ? formatPercentValue(successes / requests, 1) : "-",
      `${formatCount(successes)} 成功 · ${formatCount(stats?.failures || 0)} 失败`,
      { iconName: "check", tone: requests && successes / requests < 0.95 ? "bad" : null }),
    stat("Token", formatCompact(stats?.total_tokens || 0),
      `缓存 ${formatCompact(stats?.cached_tokens || 0)} Token`, { iconName: "cost" }),
    stat("平均耗时", formatDuration(stats?.avg_duration_ms || 0), "每次请求",
      { iconName: "clock" }),
  );
}

// rankingCard 渲染一个维度的排行榜。
//
// 用 barList 而不是表格：看板上这一排要回答"哪个占大头"，条形图的相对长度比一列
// 数字更快读。精确数字仍在右侧。
function rankingCard(dimension, valueOf, format) {
  const rows = dimensionEntries(dimension, valueOf);
  return card(
    cardHead(DIMENSION_LABELS[dimension] || dimension, badge(rangeLabel(), "muted")),
    rows.length
      ? barList(rows.slice(0, 8).map((row) => ({ name: row.name, value: row.value })),
          { format: (row) => format(row.value), emptyText: "窗口内没有请求。" })
      : empty("窗口内没有请求。", { icon: "activity" }),
  );
}

// costCard 按上游模型列出花费明细。
//
// 与「上游模型」排行榜的分工：那张按请求数排，这张按金额排。同一个模型可能请求少但
// 单价高，两张榜的顺序会不同——这正是要看出来的东西。标题里点明分组维度，否则
// "花费估算"看起来像是按本地路由名算的，而单价只对得上上游名。
function costCard() {
  const grouped = state.usage?.dimensions?.upstream_model_id;
  const cost = totalCost();
  const title = `花费估算（按${DIMENSION_LABELS.upstream_model_id}）`;
  if (!grouped || !cost) {
    const status = state.pricingLoading ? "正在读取价格目录…" : (state.pricingError || "价格目录不可用");
    return card(
      cardHead(title),
      empty("暂时无法估算花费。", { icon: "cost", hint: status }),
    );
  }
  const rows = Object.entries(grouped)
    .map(([name, stats]) => {
      const amount = sumCost(currentIndex(), { [name]: stats });
      return { name, stats, value: amount.priced > 0 ? amount.total : 0, priced: amount.priced > 0 };
    })
    .filter((row) => row.priced)
    .sort((a, b) => b.value - a.value);
  const partial = cost.priced < cost.total_count;
  return card(
    cardHead(title, badge(rangeLabel(), "muted")),
    rows.length
      ? barList(rows.slice(0, 8).map((row) => ({ name: row.name, value: row.value })),
          // 注意 barList 的 format 收到的是**整行**（charts.js:443 的 format(row)），
          // 不是 value 本身——写成 (value) => formatCost(value) 会让每根条都显示 "—"。
          { tone: "secondary", format: (row) => formatCost(row.value), emptyText: "窗口内没有可计价的请求。" })
      : empty("窗口内没有能匹配到单价的请求。", {
          icon: "cost",
          hint: "价格按上游模型名匹配 models.dev 目录；匹配不上时该条不计入金额。",
        }),
    h("div.card-foot", {},
      h("div.row-between", {},
        h("span", "合计（估算）"),
        h("strong", { title: formatCostExact(cost.total) }, formatCost(cost.total))),
      partial
        ? h("p.muted", `${cost.total_count} 个上游模型里有 ${cost.priced} 个匹配到单价，`
            + "其余未计入。金额是与 models.dev 公开单价的估算，不是账单。")
        : h("p.muted", "金额按 models.dev 公开单价估算，与实际账单可能有出入。")),
  );
}

// RECENT_COLUMNS 是最近调用明细的列。
const RECENT_COLUMNS = [
  { key: "created_at", label: "时间", render: (row) => h("span.nowrap", {}, formatDateTime(row.created_at)) },
  // 显示上游模型名（有则用之）：它与价格、与上游实际计费口径一致。本地路由名放到
  // title 里，需要时悬停可见。
  { key: "model", label: "模型", render: (row) => h("span.mono", { title: `本地路由名 ${row.model_id}` },
      truncate(row.upstream_model_id || row.model_id, 32)) },
  { key: "provider_id", label: "供应商", render: (row) => (row.provider_id
      ? h("span.mono", {}, row.provider_id)
      : h("span.muted", "—")) },
  { key: "status_code", label: "状态", numeric: true, render: (row) => statusCell(row) },
  { key: "total_tokens", label: "Token", numeric: true, render: (row) => h("span", {}, formatCompact(row.total_tokens || 0)) },
  { key: "duration_ms", label: "耗时", numeric: true, render: (row) => h("span", {}, formatDuration(row.duration_ms || 0)) },
];

// statusCell 渲染状态码。
//
// 失败要看得出是"被上游拒了"（4xx/5xx）还是"没拿到响应"（null）。重试过的请求额外
// 标一个记号：那说明这个模型/供应商当时不稳定，是排障时第一个要看的东西。
function statusCell(row) {
  const code = row.status_code;
  const tone = row.success ? "good" : (code === null || code === undefined ? "neutral" : "bad");
  const text = code === null || code === undefined ? "无响应" : String(code);
  return h("span.inline", {},
    badge(text, tone),
    row.retried ? badge("重试", "warn") : null);
}

function recentCard(usage) {
  const rows = usage?.recent_requests || [];
  const limit = 50;
  return card(
    cardHead("最近调用", badge(`${rows.length} 条`, "muted"), badge(rangeLabel(), "muted")),
    table(RECENT_COLUMNS, rows, "窗口内没有调用记录。"),
    rows.length >= limit
      ? h("div.card-foot", {}, h("p.muted", `只显示最近 ${limit} 条；更早的调用请缩短时间窗口查看。`))
      : null,
  );
}

// —— 组装 ——

function toolbar() {
  return h("div.panel-tools", {},
    state.at ? freshness(state.at) : null,
    segmented(
      USAGE_RANGES.map((item) => ({ id: item.hours, label: item.short, title: item.label })),
      state.selection,
      (id) => {
        state.selection = id === ALL_HISTORY ? ALL_HISTORY : Number(id);
        state.loading = true;
        draw();
        load();
      },
      { "aria-label": "选择统计窗口" },
    ),
    buttonNode("", {
      small: true,
      variant: "text",
      "aria-label": "刷新",
      title: "刷新",
      onClick: () => { state.loading = true; draw(); load(); },
    }, "刷新"),
    buttonNode("", {
      small: true,
      variant: "text",
      "aria-label": "退出",
      title: "清除本机保存的访问密钥",
      onClick: () => {
        saveGuestCredential("");
        state.key = "";
        state.usage = null;
        api = null;
        draw();
      },
    }, "退出"),
  );
}

function header() {
  const name = state.usage?.access_key_name || "访问密钥";
  return h("div.panel-bar", {},
    h("div.panel-id", {},
      h("span.panel-eyebrow", "访客看板"),
      h("h1.panel-name", { class: "mono" }, name),
      // 只显示尾部片段而不是完整 key：这一页常被投屏或截图，页头不该带着凭据。
      // 它足以让人确认"看的是哪一把"，又不足以被拿去调用。
      h("span.panel-hint", {}, `…${state.key.slice(-6)}`),
    ),
    toolbar(),
  );
}

function draw() {
  if (!host) return;

  if (!state.key) {
    render(host, keyForm());
    return;
  }

  const children = [header()];

  if (state.unauthorized) {
    children.push(notice("这把访问密钥无效（可能已被删除或轮换）。请在下方重新填写。", "error"));
    children.push(keyForm());
    render(host, children);
    return;
  }
  if (state.disabled) {
    children.push(notice("这把访问密钥已被停用。请联系 AMKR 管理员在「访问密钥」页重新启用。", "warn"));
    render(host, children);
    return;
  }
  if (state.error) {
    children.push(notice(`无法读取看板数据：${state.error}`, "error"));
    children.push(buttonNode("重试", { variant: "secondary", onClick: () => { state.loading = true; draw(); load(); } }));
    render(host, children);
    return;
  }
  if (state.loading || !state.usage) {
    children.push(skeleton("stats"));
    children.push(card(cardHead("用量排行"), skeleton("chart")));
    render(host, children);
    return;
  }

  const usage = state.usage;
  children.push(kpiTiles(usage));
  children.push(h("div.grid-12", {},
    h("div.col-6", {}, rankingCard("model_id",
      (stats) => Number(stats.requests) || 0, (value) => `${formatCount(value)} 次`)),
    h("div.col-6", {}, rankingCard("provider_id",
      (stats) => Number(stats.requests) || 0, (value) => `${formatCount(value)} 次`)),
  ));
  children.push(costCard());
  children.push(recentCard(usage));
  children.push(h("p.muted", {},
    "这里只显示这把访问密钥自己的流量。可用范围由管理员配置的供应商与模型清单决定；"
    + "要调整请联系管理员。"));
  render(host, children);
}
