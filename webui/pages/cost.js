// 成本：按 models.dev 单价对本机 token 用量做估算。
//
// 这一页存在的意义是把"花了多少"讲清楚，因此必须先把三件事摆在界面上，否则读数会被
// 当成账单：
//   1. **单位与来源**：单价是 USD / 100 万 token，来自 models.dev 的公开目录；
//   2. **口径**：按 token 类别分别计价（普通输入 / 缓存读 / 缓存写 / 输出），缓存价
//      缺失时回退输入价，绝不把缓存当免费；
//   3. **匹配范围**：按 upstream_model 名称匹配，所以界面要显示"有多少项匹配到了
//      单价"——没匹配到的部分不会被算进合计，更不会被当成 0。
//
// 页面顶部固定展示目录的更新时间与刷新失败告警：价格是外部事实，读数必须能追溯到
// 它是什么时候的价。

import {
  h, errorText, formatCount, formatCompact,
} from "../dom.js";
import { api } from "../api.js";
import {
  card, cardHead, stat, statGrid, notice, badge, empty, skeleton, render,
  table, buttonNode, segmented, freshness, toast,
} from "../ui.js";
import { barList } from "../charts.js";
import { TIME_RANGES, rank } from "../chart-math.js";
import {
  loadPricing, pricingStatus, currentIndex, lookupPrice, aggregateCost,
  requestCost, sumCost, formatCost, formatCostExact, formatPrice,
} from "../pricing.js";

const state = {
  hours: 24,
  snapshot: null,
  snapshotHours: null,
  snapshotError: null,
  requests: null,
  requestsError: null,
  at: null,
  loading: true,
  sort: { key: "cost", direction: "desc" },
  catalogError: null,
};

let host = null;
let xtxRef = null;
let windowToken = 0;

function rangeLabel() {
  return TIME_RANGES.find((range) => range.hours === state.hours)?.label || `${state.hours} 小时`;
}

// 默认窗口取 24 小时（而不是概览的 1 小时）：成本是"今天花了多少"这类问题，
// 一小时窗口通常只有几厘钱，看不出量级。
const COST_RANGES = [
  { hours: 1, label: "1 小时", short: "1h" },
  { hours: 24, label: "24 小时", short: "24h" },
  { hours: 72, label: "3 天", short: "3d" },
  { hours: 168, label: "7 天", short: "7d" },
  { hours: 720, label: "30 天", short: "30d" },
];

// loadWindow 取本页自己的快照与逐条明细（同一窗口，保证同屏口径一致）。
async function loadWindow(force = false) {
  if (!force && state.snapshot && state.snapshotHours === state.hours) return;
  const token = ++windowToken;
  const hours = state.hours;
  const [snapshot, requests] = await Promise.all([
    api.metrics(hours).catch((error) => ({ __error: errorText(error) })),
    // 明细的 limit 上限是 200：这里只看"最近 200 条"的成本分布，聚合金额以快照为准。
    api.requests({ hours, limit: 200 }).catch((error) => ({ __error: errorText(error) })),
  ]);
  if (token !== windowToken) return;
  state.snapshotError = snapshot.__error || null;
  state.snapshot = snapshot.__error ? null : snapshot;
  state.requestsError = requests.__error || null;
  state.requests = requests.__error ? null : (requests.items || []);
  state.snapshotHours = hours;
  state.at = new Date().toISOString();
  state.loading = false;
  draw();
}

// ensureCatalog 拉取价格目录（只拉一次，之后复用）。
async function ensureCatalog(force = false) {
  if (currentIndex() && !force) return;
  await loadPricing(() => api.pricing());
  state.catalogError = pricingStatus().error;
  draw();
}

export function renderCost(context) {
  xtxRef = context;
  const { store } = context;
  host = h("div.stack");

  if (store.health?.local_auth_enabled && !store.authorized) {
    return h("div.stack", {}, notice("需要本地鉴权 Key 才能读取用量与成本。", "warn"));
  }

  if (!state.snapshot && !state.loading) state.loading = true;
  loadWindow();
  // 目录与指标分开取：目录可能因为服务端还在下载而 503，那时不应连带把指标也显示成失败。
  ensureCatalog();
  context.onTick?.(() => { loadWindow(true); ensureCatalog(); });
  draw(true);
  return host;
}

function draw(firstPaint = false) {
  if (!host) return;
  if (!firstPaint && !host.isConnected) return;
  const { store } = xtxRef;
  const metrics = state.snapshot;
  const index = currentIndex();

  const head = h("div.page-head", {},
    h("div.page-title", {},
      h("h1", "成本"),
      h("p.sub", "按 models.dev 单价对本机 token 用量做估算（USD）。"),
    ),
    h("div.page-actions", {},
      state.at ? freshness(state.at) : null,
      segmented(COST_RANGES.map((range) => ({ id: range.hours, label: range.short, title: range.label })),
        state.hours,
        (id) => { state.hours = Number(id); state.loading = true; draw(); loadWindow(true); },
        { "aria-label": "选择统计窗口" }),
    ),
  );

  const children = [head];

  if (store.connectionError) {
    children.push(notice(`无法连接 AMKR 服务：${store.connectionError}`, "error"));
  }
  if (state.snapshotError && !metrics) {
    children.push(notice(`指标读取失败：${state.snapshotError}`, "error"));
  }
  // 目录问题单独提示：它是外部依赖，失败时本页的金额会变少（而不是变错），
  // 所以用 warn 而不是 error，并且明确说明"没算进去的部分不会被当成 0"。
  children.push(catalogNotice());

  if (!metrics) {
    children.push(skeleton("stats"));
    children.push(card(cardHead("成本构成"), skeleton("chart")));
    render(host, children);
    return;
  }

  if (!index) {
    // 没有价格目录时**不显示任何金额**：宁可只说"无定价"，也不给一堆 $0。
    children.push(card(cardHead("成本估算"),
      empty("价格目录尚不可用，暂时无法估算成本。", {
        icon: "cost",
        hint: "服务端会定期从 models.dev 刷新目录；稍后重试或检查网络后自动恢复。",
        action: buttonNode("重新获取目录", {
          variant: "secondary", small: true, iconName: "refresh",
          onClick: () => { toast("正在重新获取价格目录…"); ensureCatalog(true); },
        }),
      })));
    children.push(tokenOnlyCard(metrics));
    render(host, children);
    return;
  }

  children.push(kpiTiles(metrics, index));
  children.push(h("div.grid-12", {},
    h("div.col-7", {}, modelCostCard(metrics, index)),
    h("div.col-5", {}, compositionCard(metrics, index)),
    h("div.col-6", {}, providerCostCard(metrics, index)),
    h("div.col-6", {}, recentCostCard(index)),
    // 明细表放在最后：它不提供结论，而是让人**核对**结论——匹配错了模型价格，
    // 上面所有金额都会错，这张表是唯一能一眼看出"匹配到了哪条价"的地方。
    h("div.col-12", {}, priceTableCard(index, metrics)),
  ));

  render(host, children);
}

// catalogNotice 报告价格目录的状态（旧价 / 失败 / 尚未就绪）。
function catalogNotice() {
  const status = pricingStatus();
  if (!status.index && !status.error) return null;
  if (!status.index) {
    return notice(`价格目录尚未就绪：${status.error || "正在获取"}。金额会在目录就绪后出现。`, "warn");
  }
  if (status.stale) {
    return notice(`价格目录最近一次刷新失败（${status.staleReason}），当前显示的是上次成功获取的价格`, "warn");
  }
  return null;
}

// kpiTiles 给出三个成本读数 + 一个覆盖率读数。
//
// 覆盖率是这里最重要的一个数：它说明"合计金额覆盖了窗口内多少比例的 token"。
// 没有它，一个只匹配到 3 个模型的合计会被读成完整账单。
function kpiTiles(metrics, index) {
  const total = metrics.total || {};

  // 合计只能按 upstream_models 累加：total 是一份没有模型身份的汇总，
  // 没法乘任何单价。代价是历史数据缺 upstream 归因时合计偏低——所以下面把
  // "匹配到单价的 token 占比"也显示出来，让偏差可见而不是被隐藏。
  const upstream = metrics.upstream_models || {};
  const whole = sumCost(index, upstream);

  // 逐条明细的成本合计（只覆盖最近 200 条，与聚合口径区分开）。
  const items = state.requests || [];
  let itemTotal = 0;
  let itemsPriced = 0;
  for (const item of items) {
    const cost = requestCost(index, item);
    if (cost === null) continue;
    itemsPriced += 1;
    itemTotal += cost;
  }

  // 覆盖率按 token 维度算：匹配到单价的 token / 全部 token。
  let pricedTokens = 0;
  let allTokens = 0;
  for (const [name, stats] of Object.entries(upstream)) {
    const tokens = number(stats.total_tokens);
    allTokens += tokens;
    if (lookupPrice(index, name)) pricedTokens += tokens;
  }
  // 快照的 total 是权威总量；upstream_models 可能因为归因缺失而少于它。
  const totalTokens = number(total.total_tokens) || allTokens;
  const coverage = totalTokens ? Math.min(1, pricedTokens / totalTokens) : null;

  const coverageNote = coverage === null
    ? "窗口内没有 Token 用量"
    : coverage >= 1
      ? "全部上游模型的 Token 都匹配到单价"
      : `${Math.round(coverage * 100)}% 的 Token 匹配到单价，未匹配部分不计入金额`;

  return statGrid(
    // 一项都没计价时显示 "—" 而不是 formatCost(0) 给出的 "$0"：后者会被读成
    // "这个窗口没花钱"，而事实是"还不知道要花多少"。
    stat("估算成本", whole.priced ? formatCost(whole.total) : "—",
      `${rangeLabel()} · 覆盖 ${whole.priced}/${whole.total_count} 个上游模型`, {
        iconName: "cost", tone: whole.priced ? null : "warn",
      }),
    // 同理：明细一条都没计价时不能显示 "$0"。
    stat("最近请求成本", itemsPriced ? formatCost(itemTotal) : "—",
      items.length ? `最近 ${items.length} 条中 ${itemsPriced} 条匹配到单价` : "暂无逐条明细", {
        iconName: "activity",
      }),
    stat("Token 用量", formatCompact(total.total_tokens),
      `输入 ${formatCompact(total.prompt_tokens)} · 输出 ${formatCompact(total.completion_tokens)}`, {
        iconName: "layers",
      }),
    stat("计价覆盖率", coverage === null ? "-" : `${Math.round(coverage * 100)}%`, coverageNote, {
      iconName: "check",
      tone: coverage !== null && coverage >= 1 ? "good" : coverage !== null && coverage < 0.5 ? "warn" : null,
    }),
  );
}

// tokenOnlyCard 在目录不可用时显示用量（不含金额），让页面仍然有用。
function tokenOnlyCard(metrics) {
  const total = metrics.total || {};
  return card(
    cardHead("Token 用量", badge(rangeLabel(), "muted")),
    table([
      { key: "name", label: "项目", render: (row) => row.name },
      { key: "value", label: "数值", numeric: true, render: (row) => row.value },
    ], [
      { name: "请求", value: formatCount(total.requests) },
      { name: "输入 Token", value: formatCount(total.prompt_tokens) },
      { name: "缓存读 Token", value: formatCount(total.cache_read_input_tokens) },
      { name: "缓存写 Token", value: formatCount(total.cache_creation_input_tokens) },
      { name: "输出 Token", value: formatCount(total.completion_tokens) },
    ], "窗口内没有用量。"),
  );
}

// modelCostCard 按上游模型列出成本排行。
//
// 用 upstream_models 而不是 models：model_id 是本地路由名，价格表里没有它。
function modelCostCard(metrics, index) {
  const rows = costRank(metrics.upstream_models, index);
  const unpriced = Object.keys(metrics.upstream_models || {}).length - rows.length;
  return card(
    cardHead("模型成本排行", badge(rangeLabel(), "muted"),
      unpriced > 0 ? badge(`${unpriced} 项无定价`, "warn") : null),
    rows.length
      ? barList(rank(rows, { limit: 8, value: (row) => row.cost }), {
          tone: "primary",
          format: (row) => formatCost(row.value),
        })
      : empty("窗口内没有匹配到单价的上游模型。", {
          icon: "cost",
          hint: "成本按 upstream_model 名称匹配 models.dev 的模型 id。",
        }),
  );
}

// costRank 把 { upstream_model: stats } 转成带成本的行。
function costRank(entries, index) {
  const rows = [];
  for (const [name, stats] of Object.entries(entries || {})) {
    const cost = aggregateCost(index, name, stats);
    if (cost === null || cost <= 0) continue;
    rows.push({ name, stats, cost });
  }
  return rows.sort((a, b) => b.cost - a.cost);
}

// compositionCard 拆解成本构成：哪一部分钱花在了缓存上。
//
// 这是"成本优化"最有用的一个视图：缓存读通常比输入便宜一个数量级，
// 拆开才能看出"省了多少 / 还有多少没走缓存"。
//
// 四项按上游模型逐个折算后相加（与合计同口径）：没有模型身份的 total 乘不了单价。
function compositionCard(metrics, index) {
  const totals = { input: 0, cacheRead: 0, cacheWrite: 0, output: 0 };
  let priced = 0;
  for (const [name, stats] of Object.entries(metrics.upstream_models || {})) {
    const parts = costParts(index, name, stats);
    if (!parts) continue;
    priced += 1;
    totals.input += parts.input;
    totals.cacheRead += parts.cacheRead;
    totals.cacheWrite += parts.cacheWrite;
    totals.output += parts.output;
  }
  if (!priced) {
    return card(cardHead("成本构成", badge(rangeLabel(), "muted")),
      empty("窗口内没有匹配到单价的用量。", { icon: "cost" }));
  }

  const rows = [
    { name: "输入（未缓存）", value: totals.input },
    { name: "缓存读", value: totals.cacheRead },
    { name: "缓存写", value: totals.cacheWrite },
    { name: "输出", value: totals.output },
  ].filter((row) => row.value > 0);

  const sum = rows.reduce((acc, row) => acc + row.value, 0);
  return card(
    cardHead("成本构成", badge(rangeLabel(), "muted")),
    rows.length
      ? barList(rows, { tone: "secondary", format: (row) => formatCost(row.value) })
      : empty("窗口内没有用量。", { icon: "cost" }),
    h("div.card-foot", {},
      sum ? `四项合计 ${formatCost(sum)}（${priced} 个上游模型，按当前快照单价折算）` : "—"),
  );
}

// providerCostCard 按供应商汇总成本（归因缺失的历史数据会落到"未归因"）。
function providerCostCard(metrics, index) {
  const rows = costRank(metrics.providers, index);
  return card(
    cardHead("供应商成本", badge(`${rows.length} 项`, "muted")),
    rows.length
      ? barList(rows, { tone: "secondary", format: (row) => formatCost(row.cost) })
      : empty("没有可归因到供应商的成本数据。", {
          icon: "providers",
          hint: "供应商成本 = 该供应商下各上游模型的单价 × 用量；需要 upstream 归因。",
        }),
  );
}

// recentCostCard 列出最近几条请求的成本（逐条口径，不是聚合）。
function recentCostCard(index) {
  const items = (state.requests || []).slice(0, 12);
  if (!items.length) {
    return card(cardHead("最近请求成本", badge("0 条", "muted")),
      empty("窗口内没有请求明细。", { icon: "activity" }));
  }
  const priced = items.filter((item) => requestCost(index, item) !== null).length;
  return card(
    cardHead("最近请求成本", badge(`${priced}/${items.length} 条有定价`, "muted")),
    h("div.cost-list", {}, items.map((item) => {
      const cost = requestCost(index, item);
      const entry = lookupPrice(index, item.upstream_model_id);
      return h("div.cost-row", { title: cost === null
        ? `${item.upstream_model_id || item.model_id} 没有匹配到单价`
        : `${item.upstream_model_id} · ${formatCostExact(cost)}` },
        h("span.cost-model.mono", item.upstream_model_id || item.model_id || "—"),
        h("span.cost-tokens", formatCompact(item.total_tokens || 0)),
        h("span.cost-amount", entry ? formatCost(cost) : h("span.muted", "无定价")),
      );
    })),
    h("div.card-foot", {},
      "逐条成本按 upstream_model 单价现算；相等金额可能因四舍五入显示相同。"),
  );
}

// —— 成本拆解（与 webui/pricing.js 的 estimateCost 同口径，这里额外给出明细）——

// costParts 返回成本的四项分解；没有单价时返回 null。
function costParts(index, modelId, stats) {
  const entry = lookupPrice(index, modelId);
  if (!entry || !stats) return null;
  const prompt = number(stats.prompt_tokens);
  const completion = number(stats.completion_tokens);
  const cacheReadField = number(stats.cache_read_input_tokens);
  const cacheRead = cacheReadField > 0 ? cacheReadField : number(stats.cached_tokens);
  const cacheWrite = number(stats.cache_creation_input_tokens);
  let fresh = prompt - cacheRead - cacheWrite;
  if (fresh < 0) fresh = 0;
  const readPrice = entry.cacheRead === null ? entry.input : entry.cacheRead;
  const writePrice = entry.cacheWrite === null ? entry.input : entry.cacheWrite;
  const per = 1e6;
  return {
    input: (fresh * entry.input) / per,
    cacheRead: (cacheRead * readPrice) / per,
    cacheWrite: (cacheWrite * writePrice) / per,
    output: (completion * entry.output) / per,
  };
}

function number(value) {
  const n = Number(value);
  return Number.isFinite(n) && n > 0 ? n : 0;
}

// priceTableCard 列出当前目录里被本机用到的模型单价（可核对匹配是否正确）。
export function priceTableCard(index, metrics) {
  const rows = Object.entries(metrics?.upstream_models || {}).map(([name, stats]) => {
    const entry = lookupPrice(index, name);
    return { name, stats, entry };
  }).sort((a, b) => (b.stats.total_tokens || 0) - (a.stats.total_tokens || 0));
  if (!rows.length) return empty("窗口内没有上游模型。", { icon: "cost" });
  return table([
    { key: "name", label: "上游模型", render: (row) => h("span.mono", row.name) },
    { key: "input", label: "输入 $/1M", numeric: true, render: (row) => row.entry ? formatPrice(row.entry.input) : h("span.muted", "—") },
    { key: "output", label: "输出 $/1M", numeric: true, render: (row) => row.entry ? formatPrice(row.entry.output) : h("span.muted", "—") },
    { key: "cache", label: "缓存读 $/1M", numeric: true, render: (row) => row.entry ? formatPrice(row.entry.cacheRead === null ? row.entry.input : row.entry.cacheRead) : h("span.muted", "—") },
    { key: "cost", label: "成本", numeric: true, render: (row) => formatCost(aggregateCost(index, row.name, row.stats)) },
  ], rows, "窗口内没有上游模型。");
}
