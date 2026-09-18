// 实时活动：把"现在正在发生什么"讲清楚。
//
// 与概览的分工：概览看趋势与窗口汇总，这里看实时速率、明细请求流与分解表。
// 请求流用 /metrics/requests（真实逐条记录），不是拿日志猜的 —— 日志会被截断，
// 而请求记录带 token / 耗时 / 状态码，能直接支撑排查。

import {
  h, errorText, formatCount, formatCompact, formatDuration, formatPercent,
  formatRate, formatClockSeconds, formatDateTime, formatRelative,
} from "../dom.js";
import { api } from "../api.js";
import {
  card, cardHead, stat, statGrid, notice, badge, empty, skeleton, render,
  buttonNode, segmented, table, freshness,
} from "../ui.js";
import { icon } from "../icons.js";
import { lineChart, barList, chartSummary } from "../charts.js";
import {
  TIME_RANGES, pickBucketSeconds, bucketLabel, rank, statusGroups,
  formatPercentValue, formatCompactNumber, formatNumber, formatDurationValue,
} from "../chart-math.js";
// 成本是附加读数：目录拿不到时这一列显示 "—"，不影响请求流本身。
import {
  loadPricing, currentIndex, lookupPrice, requestCost, formatCost, formatPrice,
} from "../pricing.js";

const state = {
  hours: 1,
  series: null,
  seriesHours: null,
  seriesBucket: null,
  seriesError: null,
  requests: null,
  requestsError: null,
  requestsAt: null,
  // 本页自己按所选窗口取快照：沿用全局的 1 小时快照会让"窗口切到 7 天"时
  // 出现 7 天的图配 1 小时的 KPI 与分解表，同屏两个口径。
  snapshot: null,
  snapshotError: null,
  loading: true,
  streamFilter: "all",
  sort: { key: "requests", direction: "desc" },
  logText: null,
  logError: null,
  logAt: null,
};

let host = null;
let xtxRef = null;
let seriesToken = 0;
let logTimer = null;
let logNode = null;

function rangeLabel() {
  return TIME_RANGES.find((range) => range.hours === state.hours)?.label || `${state.hours} 小时`;
}

// 快照与序列同一窗口一次取回，保证同屏读数自洽。
async function loadSeries(force = false) {
  if (!force && state.series && state.seriesHours === state.hours) return;
  const bucket = pickBucketSeconds(state.hours);
  const token = ++seriesToken;
  const hours = state.hours;
  const [snapshot, series] = await Promise.all([
    api.metrics(hours).catch((error) => ({ __error: errorText(error) })),
    api.series(hours, bucket).catch((error) => ({ __error: errorText(error) })),
  ]);
  if (token !== seriesToken) return;
  state.snapshotError = snapshot.__error || null;
  state.snapshot = snapshot.__error ? null : snapshot;
  state.seriesError = series.__error || null;
  state.series = series.__error ? null : (series.points || []);
  state.seriesHours = hours;
  state.seriesBucket = series.bucket_seconds || bucket;
  state.loading = false;
  draw();
}

async function loadRequests() {
  try {
    const data = await api.requests({ hours: state.hours, limit: 40 });
    state.requests = data;
    state.requestsError = null;
    state.requestsAt = new Date().toISOString();
  } catch (error) {
    state.requestsError = errorText(error);
  }
}

// —— KPI ——
// 与概览一致：实时速率取 60 秒滚动窗口，其余累计量都取自同一份窗口快照。
function kpiTiles(metrics, points, bucketSeconds) {
  const total = metrics.total || {};
  const successRatio = total.requests ? total.successes / total.requests : null;
  const retryRatio = total.requests ? total.retries / total.requests : null;
  const groups = statusGroups(total.status_codes || {});

  return statGrid(
    stat("当前 RPM", formatCount(metrics.current_rpm ?? 0), "近 60 秒", { unit: "次/分", iconName: "bolt" }),
    stat("当前 TPM", formatCompact(metrics.current_tpm ?? 0), "近 60 秒", { unit: "Token/分", iconName: "activity" }),
    stat("进行中请求", formatCount(metrics.active_requests ?? 0), "正在等待上游响应", { iconName: "clock" }),
    stat("平均首字", total.requests && total.avg_first_token_ms ? formatDuration(total.avg_first_token_ms) : "-",
      total.requests ? `P0 ${formatDuration(total.min_first_token_ms)}` : "窗口内无请求", { iconName: "bolt" }),
    stat("平均耗时", total.requests ? formatDuration(total.avg_duration_ms) : "-",
      total.requests ? `最快 ${formatDuration(total.min_duration_ms)} · 最慢 ${formatDuration(total.max_duration_ms)}` : "窗口内无请求",
      { iconName: "clock", points: points || [], metricId: "latency", bucketSeconds }),
    stat("重试率", retryRatio === null ? "-" : formatPercentValue(retryRatio, 1),
      `${formatCount(total.retries)} 次重试`, {
        iconName: "refresh", tone: retryRatio !== null && retryRatio > 0.1 ? "warn" : null,
      }),
    stat("错误响应", formatCount(groups.client + groups.server),
      `4xx ${formatCount(groups.client)} · 5xx ${formatCount(groups.server)}`, {
        iconName: "alert", tone: groups.server > 0 ? "bad" : null,
      }),
    stat("缓存 Token", formatCompact(total.cached_tokens),
      `缓存率 ${formatRate(total.cached_token_rate)}`, { iconName: "filter" }),
  );
}

// —— 实时图表 ——
function liveChartCard(points, bucketSeconds) {
  const { total, latest } = chartSummary({ points, metricId: "rpm", bucketSeconds, windowHours: state.hours });
  return card(
    cardHead("实时请求速率",
      badge(`${bucketLabel(bucketSeconds)}/点`, "muted"),
      h("div.head-tools", {},
        segmented(TIME_RANGES.map((range) => ({ id: range.hours, label: range.short, title: range.label })),
          state.hours,
          (id) => { state.hours = Number(id); state.loading = true; draw(); loadSeries(true); loadRequests().then(draw); },
          { "aria-label": "选择统计窗口" }),
      ),
    ),
    state.seriesError
      ? notice(`趋势读取失败: ${state.seriesError}`, "error")
      : h("div.chart-readout", {},
          h("span.readout-main", {}, formatNumber(latest ?? 0, 1), h("small", "次/分")),
          h("span.readout-note", {}, `最近 ${rangeLabel()}平均 ${formatNumber(total ?? 0, 1)} 次/分 · 共 ${(points || []).length} 个数据点`),
        ),
    state.seriesError ? null : lineChart({
      points, metricId: "rpm", bucketSeconds, height: 240,
      ariaLabel: `实时请求速率，最近 ${rangeLabel()}`,
    }),
  );
}

// 延迟趋势：耗时与首字延迟通常差一个数量级，硬塞进同一张图会把首字曲线压平，
// 所以拆成上下两段各自成图、各自标刻度，每段自己带读数。
function latencyChartCard(points, bucketSeconds) {
  const panel = (title, metricId) => {
    const summary = chartSummary({ points, metricId, bucketSeconds, windowHours: state.hours });
    return h("div.latency-panel", {},
      h("div.latency-panel-head", {},
        h("h4", title),
        h("span.latency-panel-value", {},
          summary.latest === null || summary.latest === undefined ? "-" : formatDuration(summary.latest)),
      ),
      lineChart({
        points, metricId, bucketSeconds, height: 140, showArea: false,
        ariaLabel: `${title}趋势，最近 ${rangeLabel()}`,
      }),
    );
  };

  return card(
    cardHead("延迟趋势", badge("按请求数加权", "muted"), badge(`${bucketLabel(bucketSeconds)}/点`, "muted")),
    h("div.stack", {},
      panel("平均耗时", "latency"),
      panel("平均首字延迟", "firstToken"),
    ),
    h("div.card-foot", {}, "两项都按桶内请求数加权，而不是对桶平均值再取平均；无请求的桶没有均值可算，跨过它的虚线只表示走势延续。"),
  );
}

// —— 请求流 ——
// 直连真实请求记录，可按成功/失败/重试过滤。
function filteredRequests() {
  const items = state.requests?.items || [];
  if (state.streamFilter === "failure") return items.filter((item) => !item.success);
  if (state.streamFilter === "retry") return items.filter((item) => item.retried);
  if (state.streamFilter === "success") return items.filter((item) => item.success);
  return items;
}

function streamCard() {
  if (state.requestsError) {
    return card(cardHead("请求流"), notice(`请求明细读取失败: ${state.requestsError}`, "error"));
  }
  const items = filteredRequests();
  const all = state.requests?.items || [];
  const counts = {
    all: all.length,
    success: all.filter((item) => item.success).length,
    failure: all.filter((item) => !item.success).length,
    retry: all.filter((item) => item.retried).length,
  };
  const options = [
    { id: "all", label: `全部 ${counts.all}` },
    { id: "success", label: `成功 ${counts.success}` },
    { id: "failure", label: `失败 ${counts.failure}` },
    { id: "retry", label: `重试 ${counts.retry}` },
  ];

  return card(
    cardHead("请求流",
      badge(`最近 ${all.length} 条`, "muted"),
      state.requestsAt ? freshness(state.requestsAt, { prefix: "拉取于" }) : null,
    ),
    // 过滤器放在卡头下方而不是塞进卡头：卡片只占 5 列时，
    // 四个筛选项会把标题挤成竖排（中文标题尤其明显）。
    h("div.toolbar", { style: { marginBottom: "8px" } },
      segmented(options, state.streamFilter, (id) => { state.streamFilter = id; draw(); },
        { "aria-label": "按结果过滤请求流" }),
    ),
    items.length
      ? h("div.stream", {}, items.map(streamRow))
      : empty("该条件下暂无请求记录。", { icon: "activity", hint: "窗口内没有请求时，这里会是空的。" }),
  );
}

function streamRow(item) {
  const tone = !item.success ? "is-failure" : item.retried ? "is-retry" : "";
  const status = item.status_code ?? "无响应";
  return h(`div.stream-row${tone ? `.${tone}` : ""}`, {},
    h("span.stream-bar"),
    h("span.stream-time", { title: item.created_at }, formatClockSeconds(item.created_at)),
    h("div.stream-main", {},
      h("span.stream-model", { title: item.model_id }, item.model_id),
      h("span.stream-meta", {},
        [
          item.caller_type === "visitor" ? "访客" : "本机",
          item.key_name,
          item.provider_id || null,
          `HTTP ${status}`,
          item.retried ? "已重试" : null,
        ].filter(Boolean).join(" · ")),
    ),
    h("span.stream-tokens", {}, item.total_tokens ? formatCompact(item.total_tokens) : "—"),
    h("span.stream-latency", {}, formatDuration(item.duration_ms)),
    h("span.stream-cost", { title: costTitle(item) }, costText(item)),
  );
}

// costText 给出该条请求的估算成本。
//
// 拿不到价格时显示 "—" 而不是 "$0"：0 会被读成"这次请求免费"，而事实是"不知道"。
// 目录尚未加载（首屏）时同样显示 "—"，避免先闪一堆 $0 再变成真实值。
function costText(item) {
  const index = currentIndex();
  if (!index) return "—";
  return formatCost(requestCost(index, item));
}

// costTitle 是成本列的悬停说明：把匹配到的单价写出来，方便核对匹配是否正确。
function costTitle(item) {
  const index = currentIndex();
  const name = item.upstream_model_id;
  if (!index) return "价格目录尚未就绪";
  if (!name) return "该请求没有 upstream_model 归因，无法匹配价格";
  const entry = lookupPrice(index, name);
  if (!entry) return `${name} 未在 models.dev 目录中匹配到价格`;
  return `${name} · 输入 ${formatPrice(entry.input)} / 输出 ${formatPrice(entry.output)} USD per 1M token`;
}

// —— 分解表 ——
const COLUMNS = [
  { key: "name", label: "", render: (row) => h("span.mono", { title: row.name }, row.name), sortValue: (row) => row.name },
  { key: "requests", label: "请求", numeric: true, render: (row) => formatCount(row.stats.requests), sortValue: (row) => row.stats.requests },
  { key: "successes", label: "成功", numeric: true, render: (row) => formatCount(row.stats.successes), sortValue: (row) => row.stats.successes },
  { key: "failures", label: "失败", numeric: true, render: (row) => row.stats.failures
      ? h("span", { style: { color: "var(--md-error)" } }, formatCount(row.stats.failures))
      : "0", sortValue: (row) => row.stats.failures },
  { key: "rate", label: "成功率", numeric: true, render: (row) => formatPercent(row.stats.successes, row.stats.requests), sortValue: (row) => (row.stats.requests ? row.stats.successes / row.stats.requests : -1) },
  { key: "tokens", label: "Token", numeric: true, render: (row) => formatCompact(row.stats.total_tokens), sortValue: (row) => row.stats.total_tokens },
  { key: "cache", label: "缓存率", numeric: true, render: (row) => formatRate(row.stats.cached_token_rate), sortValue: (row) => row.stats.cached_token_rate },
  { key: "duration", label: "平均耗时", numeric: true, render: (row) => formatDuration(row.stats.avg_duration_ms), sortValue: (row) => row.stats.avg_duration_ms },
];

function breakdownCard(title, entries, keyLabel, emptyText) {
  const rows = Object.entries(entries || {})
    .map(([name, stats]) => ({ name, stats }))
    .sort(comparator());
  if (!rows.length) return card(cardHead(title, badge("0 项", "muted")), empty(emptyText, { icon: "logs" }));
  // 总量行：让"表里各项加起来是多少"有对照，避免只看到分布看不到规模。
  const totalRow = rows.reduce((acc, row) => ({
    requests: acc.requests + (row.stats.requests || 0),
    successes: acc.successes + (row.stats.successes || 0),
    failures: acc.failures + (row.stats.failures || 0),
    total_tokens: acc.total_tokens + (row.stats.total_tokens || 0),
    cached_tokens: acc.cached_tokens + (row.stats.cached_tokens || 0),
    prompt_tokens: acc.prompt_tokens + (row.stats.prompt_tokens || 0),
    total_duration_ms: acc.total_duration_ms + (row.stats.total_duration_ms || 0),
  }), { requests: 0, successes: 0, failures: 0, total_tokens: 0, cached_tokens: 0, prompt_tokens: 0, total_duration_ms: 0 });
  const totalStats = {
    ...totalRow,
    cached_token_rate: totalRow.prompt_tokens ? totalRow.cached_tokens / totalRow.prompt_tokens : 0,
    avg_duration_ms: totalRow.requests ? totalRow.total_duration_ms / totalRow.requests : 0,
  };

  return card(
    cardHead(title,
      badge(`${rows.length} 项`, "muted"),
      badge(rangeLabel(), "muted"),
    ),
    table(COLUMNS, rows, emptyText, {
      sort: state.sort,
      onSort: (key) => {
        const direction = state.sort.key === key && state.sort.direction === "desc" ? "asc" : "desc";
        state.sort = { key, direction };
        draw();
      },
    }),
    h("div.card-foot", {},
      h("div.row-between", {},
        h("span", `${keyLabel}合计`),
        h("span.inline", {},
          h("strong", formatCount(totalStats.requests)),
          h("span.muted", " 次请求 · "),
          h("strong", formatCompact(totalStats.total_tokens)),
          h("span.muted", " Token · 成功率 "),
          h("strong", formatPercent(totalStats.successes, totalStats.requests)),
        ),
      ),
    ),
  );
}

function comparator() {
  const { key, direction } = state.sort;
  const sign = direction === "asc" ? 1 : -1;
  const column = COLUMNS.find((item) => item.key === key);
  const pick = column?.sortValue || ((row) => row.stats.requests);
  return (a, b) => {
    const left = pick(a);
    const right = pick(b);
    if (typeof left === "string" || typeof right === "string") {
      return sign * String(left).localeCompare(String(right), "zh-CN");
    }
    return sign * ((left ?? 0) - (right ?? 0));
  };
}

function flattenKeys(keys) {
  const flat = {};
  for (const [model, byKey] of Object.entries(keys || {})) {
    for (const [key, stats] of Object.entries(byKey || {})) flat[`${model} / ${key}`] = stats;
  }
  return flat;
}

// —— 服务日志 ——
function logCard() {
  const pre = h("pre.log-panel", { tabindex: "0" }, "正在读取服务日志…");
  logNode = pre;
  paintLog();
  return card(
    cardHead("服务日志",
      badge("最近 64 KiB", "muted"),
      badge("2 秒刷新", "muted"),
      h("div.head-tools", {},
        buttonNode("立即刷新", {
          variant: "text", small: true, iconName: "refresh",
          onClick: () => { loadLog(); },
        }),
      ),
    ),
    pre,
  );
}

async function loadLog() {
  try {
    const data = await api.logs();
    if (data.error) {
      state.logError = data.error;
      state.logText = null;
    } else {
      state.logError = null;
      state.logText = data.text || "";
    }
    state.logAt = new Date().toISOString();
  } catch (error) {
    state.logError = errorText(error);
  }
  paintLog();
}

function paintLog() {
  const pre = logNode;
  if (!pre || !pre.isConnected) return;
  // 只有用户本来就贴在底部时才自动跟滚，否则会打断向上翻阅。
  const pinned = pre.scrollHeight - pre.scrollTop - pre.clientHeight <= 8;
  if (state.logError) {
    pre.replaceChildren(h("div.lv-error", `日志暂不可用: ${state.logError}`));
    return;
  }
  const lines = (state.logText || "").split(/\r?\n/);
  pre.replaceChildren(...(lines.length ? lines.map(lineNode) : [document.createTextNode("日志为空。")]));
  if (pinned) pre.scrollTop = pre.scrollHeight;
}

function lineNode(line) {
  const match = /\b(trace|debug|info|warn(?:ing)?|error|fatal|critical)\b/i.exec(line);
  if (!match) return h("div", line);
  const level = match[1].toLowerCase();
  const tone = /error|fatal|critical/.test(level) ? "lv-error"
    : /warn/.test(level) ? "lv-warning"
    : level === "info" ? "lv-info" : "lv-debug";
  return h(`div.${tone}`, line);
}

export function renderActivity(context) {
  xtxRef = context;
  const { store } = context;
  host = h("div.stack");

  if (store.health?.local_auth_enabled && !store.authorized) {
    return h("div.stack", {}, notice("需要本地鉴权 Key 才能读取统计。", "warn"));
  }

  if (state.seriesHours !== state.hours) {
    state.loading = true;
    loadSeries();
  }
  loadRequests().then(draw);
  // 价格目录只拉一次，之后复用（见 webui/pricing.js）；失败也不阻断请求流。
  loadPricing(() => api.pricing()).then(draw).catch(() => {});
  // 每次进入页面都重新起日志轮询并重新注册订阅：上一次离开时已经把它们清掉了。
  startLogPolling();
  context.onTick?.(() => { loadRequests().then(draw); });
  // 切页时释放定时器 —— 不做的话离开这一页后日志仍会每 2 秒请求一次。
  context.onLeave?.(stopLogPolling);
  draw();
  return host;
}

function startLogPolling() {
  stopLogPolling();
  loadLog();
  logTimer = setInterval(loadLog, 2000);
}

function stopLogPolling() {
  if (logTimer !== null) {
    clearInterval(logTimer);
    logTimer = null;
  }
}

function draw() {
  if (!host || !host.isConnected) return;
  const { store } = xtxRef;
  const metrics = state.snapshot;
  const points = state.series || [];
  const bucketSeconds = state.seriesBucket || pickBucketSeconds(state.hours);

  const children = [
    h("div.page-head", {},
      h("div.page-title", {},
        h("h1", "实时活动"),
        h("p.sub", "实时速率、逐条请求明细与按维度的用量分解。"),
      ),
      h("div.page-actions", {},
        state.requestsAt ? freshness(state.requestsAt) : null,
        metadataBadges(metrics),
      ),
    ),
  ];

  if (store.connectionError) children.push(notice(`无法连接 AMKR 服务：${store.connectionError}`, "error"));
  if (state.snapshotError && !metrics) children.push(notice(`指标读取失败：${state.snapshotError}`, "error"));
  if (state.seriesError) children.push(notice(`趋势数据读取失败：${state.seriesError}`, "error"));

  if (!metrics) {
    children.push(skeleton("stats"));
    children.push(card(cardHead("实时请求速率"), skeleton("chart")));
    children.push(card(cardHead("请求流"), skeleton("table", 6)));
  } else {
    children.push(kpiTiles(metrics, points, bucketSeconds));
    children.push(liveChartCard(points, bucketSeconds));
    const flatKeys = flattenKeys(metrics.keys);
    // 同一个栅格承载全部卡片，间隙统一为 gap；与概览页保持一致的排版规则。
    children.push(h("div.grid-12", {},
      h("div.col-7", {}, latencyChartCard(points, bucketSeconds)),
      h("div.col-5", {}, streamCard()),
      h("div.col-6", {}, breakdownCard("模型用量", metrics.models, "模型", "窗口内没有模型调用。")),
      h("div.col-6", {}, breakdownCard("调用方用量", metrics.caller_types, "调用方", "窗口内没有调用方数据。")),
      h("div.col-12", {}, breakdownCard("模型 / Key 用量", flatKeys, "条目", "窗口内没有 Key 调用数据。")),
      h("div.col-4", {}, upstreamCard(metrics)),
      h("div.col-8", {}, logCard()),
    ));
  }

  render(host, children);
  // 重建后 logNode 指向被丢弃的节点，重新绑定到新的 <pre>。
  logNode = host.querySelector?.(".log-panel") || logNode;
  paintLog();
  markStreamFresh();
}

function markStreamFresh() {
  // 重新绘制后把滚动条定位到最新一条：请求流默认看"刚发生的"。
  const stream = host?.querySelector?.(".stream");
  if (stream) stream.scrollTop = 0;
}

function metadataBadges(metrics) {
  if (!metrics) return null;
  const tone = { green: "good", yellow: "warn", red: "bad" }[metrics.router_status] || "muted";
  const label = { green: "路由正常", yellow: "路由警告", red: "路由异常" }[metrics.router_status] || "路由空闲";
  return h("div.inline", {},
    badge(label, tone),
    badge(`计数口径 ${metrics.count_semantics === "upstream_attempt" ? "上游尝试" : metrics.count_semantics || "未知"}`, "muted"),
  );
}

function upstreamCard(metrics) {
  const rows = rank(metrics.upstream_models, {
    limit: 8,
    value: (stats) => stats.requests,
  });
  const providers = rank(metrics.providers, { limit: 8, value: (stats) => stats.requests });
  return card(
    cardHead("上游分布", badge(rangeLabel(), "muted")),
    h("div.stack", {},
      h("div", {},
        h("h4", { class: "muted", style: { marginBottom: "8px" } }, "按上游模型"),
        barList(rows, { emptyText: "没有上游模型归因数据。", format: (row) => `${formatCount(row.value)} 次` }),
      ),
      providers.length
        ? h("div", {},
            h("h4", { class: "muted", style: { marginBottom: "8px" } }, "按供应商"),
            barList(providers, {
              tone: "secondary",
              format: (row) => `${formatCompact(row.stats.total_tokens)} Token`,
            }),
          )
        : h("p.muted", "历史数据的供应商归因可能为空（v4 起不再写入模型池归因）。"),
    ),
  );
}
