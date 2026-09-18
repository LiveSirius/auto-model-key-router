// 概览：监控看板的首页。
//
// 数据来源分两类，必须说清楚，否则读数会被误读：
//   1. /metrics（小时=1）—— 窗口汇总与实时速率，KPI 用这里；
//   2. /metrics/series —— 时间序列，趋势图用这里。桶宽由 chart-math 选最小的
//      可行值（后端上限 500 点），所以 1 小时窗口是 15 秒一个桶。
// 两张图各自标注自己的时间窗与桶宽，避免"图上是 15 秒桶、数字是 1 小时窗口"混淆。

import {
  h, errorText, copyText, formatCount, formatCompact, formatDuration,
  formatPercent, formatRate,
} from "../dom.js";
import { api } from "../api.js";
import {
  card, cardHead, stat, statGrid, notice, badge, empty, skeleton, render,
  buttonNode, segmented, freshness, progressBar, toast,
} from "../ui.js";
import { icon } from "../icons.js";
import { lineChart, stackedBars, donut, barList, legend, chartSummary, heatmap } from "../charts.js";
import {
  TIME_RANGES, pickBucketSeconds, bucketLabel, METRIC_MAP, HEATMAP_METRICS,
  rank, statusGroups, formatPercentValue, formatCompactNumber, formatNumber,
  trend as computeTrend,
} from "../chart-math.js";

// 热力图固定看 7 天：它的价值就在于"周内节律"（工作日 vs 周末、白天 vs 夜间），
// 跟着页头的 1h/6h 窗口走就没有可比性了。桶宽固定半小时，正好一格一个桶
// （7 × 48 = 336 格，仍在后端 500 点上限内：168*3600/1800 + 1 = 337）。
export const HEATMAP_WINDOW_HOURS = 168;
export const HEATMAP_BUCKET_SECONDS = 1800;
// 尾桶是半小时聚合值，半分钟内不会变，没必要跟着 10 秒轮询重算 337 个桶。
const HEATMAP_TTL_MS = 60000;

// 页面级状态：时间范围与主图指标是用户选择，需在轮询重绘间保持。
const state = {
  hours: 1,
  metric: "rpm",
  series: null,
  seriesHours: null,
  seriesBucket: null,
  seriesError: null,
  // 本页自己按所选窗口取快照，而不是复用全局的 1 小时快照：
  // 否则把窗口切到 7 天时，折线是 7 天、排行与状态分布却仍是 1 小时，
  // 同一屏出现两个口径的"总量"。
  snapshot: null,
  snapshotHours: null,
  snapshotError: null,
  snapshotAt: null,
  loading: true,
  trendMode: "line",
  // 热力图独立于上面的窗口：自己的 7 天数据、自己的指标、自己的刷新节奏。
  heatMetric: "requests",
  heatPoints: null,
  heatError: null,
  heatAt: null,
  heatToken: 0,
};

let host = null;
let xtxRef = null;
let windowToken = 0;

function rangeLabel() {
  return TIME_RANGES.find((range) => range.hours === state.hours)?.label || `${state.hours} 小时`;
}

// 快照与序列必须同一个窗口、同一次请求周期内取，保证同屏数字自洽。
async function loadWindow(force = false) {
  if (!force && state.series && state.seriesHours === state.hours) return;
  const bucket = pickBucketSeconds(state.hours);
  const token = ++windowToken;
  const hours = state.hours;
  const [snapshot, series] = await Promise.all([
    api.metrics(hours).catch((error) => ({ __error: errorText(error) })),
    api.series(hours, bucket).catch((error) => ({ __error: errorText(error) })),
  ]);
  if (token !== windowToken) return; // 期间用户又换了范围，丢弃过期响应
  state.snapshotError = snapshot.__error || null;
  state.snapshot = snapshot.__error ? null : snapshot;
  state.seriesError = series.__error || null;
  state.series = series.__error ? null : (series.points || []);
  state.seriesHours = hours;
  state.snapshotHours = hours;
  state.seriesBucket = series.bucket_seconds || bucket;
  state.snapshotAt = new Date().toISOString();
  state.loading = false;
  draw();
}

// 热力图数据与主图分开取：它固定 7 天，且不需要每次都重算。
// TTL 对失败同样生效（heatAt 记录的是"上次尝试"），否则接口报错时每次轮询都会重试。
async function loadHeatmap(force = false) {
  if (!force && state.heatAt && Date.now() - Date.parse(state.heatAt) < HEATMAP_TTL_MS) return;
  const token = ++state.heatToken;
  const series = await api
    .series(HEATMAP_WINDOW_HOURS, HEATMAP_BUCKET_SECONDS)
    .catch((error) => ({ __error: errorText(error) }));
  if (token !== state.heatToken) return; // 已有更新的请求在途，丢弃过期响应
  state.heatError = series.__error || null;
  state.heatPoints = series.__error ? null : (series.points || []);
  state.heatAt = new Date().toISOString();
  draw();
}

// —— KPI 区 ——
// 实时速率（当前 RPM/TPM）来自 /metrics 的 60 秒滚动窗口，与所选统计窗口无关；
// 其余累计量都取自同一份窗口快照，保证同屏数字出自同一口径。
function kpiTiles(metrics, points, bucketSeconds) {
  const total = metrics.total || {};
  const successRatio = total.requests ? total.successes / total.requests : null;
  const cacheRatio = total.prompt_tokens ? total.cached_tokens / total.prompt_tokens : null;
  const currentRpm = metrics.current_rpm ?? 0;
  const currentTpm = metrics.current_tpm ?? 0;
  // 环比只看"已完结"的桶，否则末尾残桶会把结论拖偏。
  const rpmTrend = computeTrend((points || []).map((point) => ({
    value: point.complete === false ? null : (point.requests || 0) * (60 / (bucketSeconds || 60)),
  })));

  const statusTone = metrics.router_status === "red" ? "bad"
    : metrics.router_status === "yellow" ? "warn" : null;

  return statGrid(
    stat("当前 RPM", formatCount(currentRpm), `近 ${metrics.rate_window_seconds || 60} 秒窗口`, {
      unit: "次/分", iconName: "bolt", tone: statusTone,
    }),
    stat("当前 TPM", formatCompact(currentTpm), "近 60 秒 Token 速率", {
      unit: "Token/分", iconName: "activity",
    }),
    stat("窗口请求", formatCount(total.requests), `最近 ${rangeLabel()}`, {
      iconName: "layers", points: points || [], metricId: "rpm", bucketSeconds,
      // 流量涨跌本身不分好坏，用中性色；否则一次正常高峰会被标成红色告警。
      trendChange: rpmTrend?.change ?? null, trendPolarity: "neutral",
    }),
    stat("成功率", successRatio === null ? "-" : formatPercentValue(successRatio, 1),
      `${formatCount(total.successes)} 成功 · ${formatCount(total.failures)} 失败`, {
        iconName: "check",
        tone: successRatio !== null && successRatio < 0.95 ? "bad" : null,
      }),
    stat("Token 用量", formatCompact(total.total_tokens),
      `输入 ${formatCompact(total.prompt_tokens)} · 输出 ${formatCompact(total.completion_tokens)}`, {
        iconName: "layers", points: points || [], metricId: "tpm", bucketSeconds,
      }),
    stat("缓存命中率", cacheRatio === null ? "-" : formatPercentValue(cacheRatio, 1),
      `缓存 ${formatCompact(total.cached_tokens)} Token`, {
        iconName: "filter",
        tone: cacheRatio !== null && cacheRatio > 0.3 ? "good" : null,
      }),
    stat("平均耗时", total.requests ? formatDuration(total.avg_duration_ms) : "-",
      total.requests ? `最快 ${formatDuration(total.min_duration_ms)} · 最慢 ${formatDuration(total.max_duration_ms)}` : "窗口内无请求", {
        iconName: "clock",
      }),
    stat("平均首字", total.requests && total.avg_first_token_ms ? formatDuration(total.avg_first_token_ms) : "-",
      `${formatCount(metrics.active_requests ?? 0)} 个请求进行中`, {
        iconName: "activity",
      }),
  );
}

// —— 主趋势图 ——
function trendCard() {
  const points = state.series || [];
  const bucketSeconds = state.seriesBucket || pickBucketSeconds(state.hours);
  const metric = METRIC_MAP[state.metric] || METRIC_MAP.rpm;
  const { total, latest } = chartSummary({ points, metricId: state.metric, bucketSeconds, windowHours: state.hours });

  const body = state.seriesError
    ? notice(`趋势数据读取失败: ${state.seriesError}`, "error")
    : state.loading && !points.length
      ? skeleton("chart")
      : h("div.stack", {},
          h("div.chart-readout", {},
            h("span.readout-main", {},
              state.metric === "tokens" ? formatCompactNumber(total ?? 0) : formatNumber(total ?? 0, 1),
              h("small", state.metric === "tokens" ? "Token" : metric.unit),
            ),
            h("span.readout-note", {},
              state.metric === "tokens"
                ? `最近 ${rangeLabel()}合计`
                : `最近 ${rangeLabel()}平均 · 最新 ${metric.format(latest ?? 0)}`),
            h("span.spacer"),
            h("span.readout-note", {}, `${bucketLabel(bucketSeconds)}/点 · 共 ${points.length} 点`),
          ),
          lineChart({
            points, metricId: state.metric, bucketSeconds, height: 280,
            ariaLabel: `${metric.label}趋势，最近 ${rangeLabel()}`,
          }),
        );

  return card(
    cardHead("流量趋势",
      badge(`${rangeLabel()}窗口`, "muted"),
      h("div.head-tools", {},
        segmented(
          [{ id: "rpm", label: "请求速率" }, { id: "tpm", label: "Token 速率" },
           { id: "tokens", label: "Token 用量" }, { id: "latency", label: "耗时" },
           { id: "success", label: "成功率" }],
          state.metric,
          (id) => { state.metric = id; draw(); },
          { "aria-label": "选择趋势指标" },
        ),
      ),
    ),
    h("p.muted", { style: { marginBottom: "12px" } }, metric.hint),
    body,
  );
}

// —— 构成与分布 ——
// 用窗口快照而不是序列求和：这两个数字会和 KPI 瓦片并排出现，
// 不同来源的小数位差异会让人怀疑数据不准。
function compositionCard(metrics) {
  const total = metrics.total || {};
  const prompt = total.prompt_tokens || 0;
  const completion = total.completion_tokens || 0;
  const grandTotal = prompt + completion;
  return card(
    cardHead("Token 构成", badge(rangeLabel(), "muted")),
    h("div.stack", {},
      donut({
        ratio: grandTotal ? completion / grandTotal : null,
        label: "输出占比",
        tone: "secondary",
        size: 152,
        caption: `合计 ${formatCompact(grandTotal)} Token`,
      }),
      legend([
        { label: "输入", value: formatCompact(prompt), tone: "primary" },
        { label: "输出", value: formatCompact(completion), tone: "secondary" },
        { label: "缓存读", value: formatCompact(total.cached_tokens || 0), tone: "neutral" },
      ]),
    ),
  );
}

function statusCard(metrics) {
  const groups = statusGroups(metrics.total?.status_codes || {});
  const total = groups.ok + groups.client + groups.server + groups.other;
  const rows = [
    { label: "2xx 成功", value: groups.ok, tone: "good" },
    { label: "4xx 客户端错误", value: groups.client, tone: "warn" },
    { label: "5xx 服务端错误", value: groups.server, tone: "bad" },
    { label: "其它状态", value: groups.other, tone: "neutral" },
  ].filter((row) => row.value > 0);

  return card(
    cardHead("响应状态分布", badge(`最近 ${rangeLabel()}`, "muted")),
    total
      ? h("div.stack", {},
          h("div.status-bar", { role: "img", "aria-label": "响应状态占比" },
            rows.map((row) => h("span", {
              class: `status-seg tone-${row.tone}`,
              style: { flex: String(row.value) },
              title: `${row.label} ${formatCount(row.value)}`,
            }))),
          h("div.stack.tight", {}, rows.map((row) =>
            h("div.row-between", {},
              h("span.inline", {},
                h("i", { class: `swatch tone-${row.tone === "good" ? "secondary" : row.tone === "neutral" ? "neutral" : row.tone}` }),
                row.label,
              ),
              h("span.inline", {},
                h("strong", formatCount(row.value)),
                h("span.muted", ` · ${formatPercent(row.value, total)}`),
              ),
            ))),
        )
      : empty("窗口内没有带状态码的请求。", { icon: "activity" }),
  );
}

// 请求流转：本次窗口内的"尝试"如何收敛成成功。
// 刻意不用箭头列：那会让人误读成顺序漏斗（请求→重试→失败→成功），
// 而成功率/重试率/失败率是同一批请求的三个侧面，用横向占比条最诚实。
function waterfallCard(metrics) {
  const total = metrics.total || {};
  const requests = total.requests || 0;
  const retries = total.retries || 0;
  const failures = total.failures || 0;
  const successes = total.successes || 0;

  const rows = [
    { label: "成功", value: successes, tone: "secondary", hint: "上游返回 2xx" },
    { label: "重试", value: retries, tone: "warn", hint: "同一次调用换了 Key/上游再试" },
    { label: "失败", value: failures, tone: "bad", hint: "所有尝试均未成功" },
  ];

  return card(
    cardHead("请求结果", badge("按上游尝试计数", "muted"), badge(`最近 ${rangeLabel()}`, "muted")),
    h("div.flow-total", {},
      h("span.flow-total-value", formatCount(requests)),
      h("span.flow-total-label", "次上游尝试"),
    ),
    h("div.flow-rows", {}, rows.map((row) => {
      const ratio = requests ? row.value / requests : 0;
      return h("div.flow-row", {},
        h("div.flow-row-head", {},
          h("span.inline", {}, h("i", { class: `swatch tone-${row.tone === "secondary" ? "secondary" : row.tone}` }), row.label),
          h("span.inline", {},
            h("strong", formatCount(row.value)),
            h("span.muted", ` · ${formatPercent(row.value, requests)}`),
          ),
        ),
        progressBar(ratio * 100, row.tone === "secondary" ? "good" : row.tone),
        h("span.flow-row-hint", row.hint),
      );
    })),
    h("div.card-foot", {},
      `平均耗时 ${formatDuration(total.avg_duration_ms)} · 缓存率 ${formatRate(total.cached_token_rate)}`),
  );
}

function rankingCard(title, entries, keyLabel, options = {}) {
  const rows = rank(entries, { limit: options.limit || 6, value: options.value });
  return card(
    cardHead(title, badge(`${Object.keys(entries || {}).length} 项`, "muted")),
    barList(rows, {
      tone: options.tone || "primary",
      emptyText: `窗口内没有${keyLabel}数据。`,
      format: options.format || ((row) => `${formatCount(row.value)} 次`),
    }),
  );
}

// —— 统一模型入口卡 ——
function unifiedCard() {
  const { store, navigate } = xtxRef;
  const unified = store.health?.unified_model;
  const target = unified?.default?.primary;
  const description = unified
    ? (target?.key ? `固定 Key · ${target.key}` : "自动路由（按 Key 池顺序）")
    : "未启用";
  return h("div.card.is-lift", {
    role: "link", tabindex: "0", "aria-label": "查看统一模型配置",
    onClick: () => navigate("unified"),
    onKeydown: (event) => { if (event.key === "Enter" || event.key === " ") { event.preventDefault(); navigate("unified"); } },
  },
    cardHead("统一模型入口",
      unified ? badge("已启用", "good") : badge("未启用", "muted"),
      h("div.head-tools", {}, icon("chevron", { size: 18, class: "muted" })),
    ),
    h("div.stack.tight", {},
      h("div.unified-target", {},
        h("span.muted", "主模型"),
        h("strong", target?.model || "—"),
      ),
      h("div", {}, h("span.muted", "路由方式："), description),
      unified?.default?.fallback?.model
        ? h("div", {}, h("span.muted", "回退模型："), h("span.mono", unified.default.fallback.model))
        : h("div.muted", "未配置回退模型"),
      unified?.image?.primary?.model
        ? h("div", {}, h("span.muted", "图像模型："), h("span.mono", unified.image.primary.model))
        : null,
      unified?.embeddings?.primary?.model
        ? h("div", {}, h("span.muted", "嵌入模型："), h("span.mono", unified.embeddings.primary.model))
        : null,
    ),
    h("div.card-foot", {},
      h("span", `可用模型 ${(store.health?.models || []).length} 个`),
    ),
  );
}

function runtimeCard() {
  const { store, refreshHealth } = xtxRef;
  const health = store.health || {};
  const native = Object.values(health.native_endpoint_states || {});
  const ok = native.filter((state) => state?.supported).length;
  const rows = [
    ["监听地址", health.base_url || "—"],
    ["版本", health.version ? `v${health.version}` : "—"],
    ["配置路径", health.config_path || "—"],
    ["本地鉴权", health.local_auth_enabled ? "已启用" : "未启用"],
    ["访客访问", health.visitor_access_enabled ? `已启用 · ${health.visitor_key_count} 个 Key` : "未启用"],
  ];
  return card(
    cardHead("运行状态",
      health.local_auth_enabled ? badge("鉴权已启用", "good") : badge("鉴权未启用", "warn"),
      h("div.head-tools", {},
        buttonNode("刷新", { variant: "text", small: true, iconName: "refresh", onClick: () => refreshHealth() }),
      ),
    ),
    h("dl.kv", {}, rows.flatMap(([key, value]) => [h("dt", key), h("dd", {}, value)])),
    native.length
      ? h("div.card-foot", {},
          h("div.row-between", {},
            h("span", `原生端点可用 ${ok} / ${native.length}`),
            h("span", { class: "muted" }, "响应/Anthropic 原生转发能力"),
          ),
          h("div", { style: { marginTop: "8px" } }, progressBar(native.length ? (ok / native.length) * 100 : 0, ok === native.length ? "good" : "primary")),
        )
      : null,
  );
}

// —— 热力图：一周的用量节律 ——
// 与页头窗口无关（见 HEATMAP_WINDOW_HOURS 的说明），所以徽标自己写死"7 天"，
// 不跟 rangeLabel() 走 —— 否则切到 1h 时图上写着"最近 1 小时"、矩阵却是整周。
function heatmapCard() {
  const metric = HEATMAP_METRICS.find((item) => item.id === state.heatMetric) || HEATMAP_METRICS[0];
  const points = state.heatPoints || [];
  const body = state.heatError
    ? notice(`热力图数据读取失败: ${state.heatError}`, "error")
    : !state.heatPoints
      ? skeleton("chart")
      : heatmap({
          points,
          metric,
          ariaLabel: `最近 7 天的${metric.label}热力图，按周内与半小时分布`,
        });

  return card(
    cardHead("用量热力图",
      badge("最近 7 天", "muted"),
      badge("30 分钟/格", "muted"),
      h("div.head-tools", {},
        segmented(HEATMAP_METRICS.map((item) => ({ id: item.id, label: item.label })),
          state.heatMetric,
          (id) => { state.heatMetric = id; draw(); },
          { "aria-label": "选择热力图指标" }),
      ),
    ),
    body,
  );
}

// —— 页面组装 ——
export function renderOverview(context) {
  xtxRef = context;
  host = h("div.stack");
  if (context.store.health?.local_auth_enabled && !context.store.authorized) {
    return h("div.stack", {}, notice("需要本地鉴权 Key 才能读取用量。", "warn"));
  }
  if (!state.snapshot && !state.loading) state.loading = true;
  if (state.seriesHours !== state.hours) loadWindow();
  loadHeatmap();
  // 指标轮询时刷新本页窗口数据（快照 + 序列一起，保持同屏口径一致）。
  // 热力图有自己的 60 秒 TTL，force 会绕过它 —— 但那时桶内容其实没变，
  // 所以只在窗口数据真正变化时顺带刷新，避免每 10 秒重画 336 个格子。
  context.onTick?.(() => { loadWindow(true); loadHeatmap(); });
  // 同步首绘必须绕过"已挂载"守卫：app.js 是**先**拿本函数返回的节点、**后**mount 进
  // 文档的（renderContent 里 page.render(ctx()) 在前，mount 在后），此刻 host 还不在
  // 文档里，isConnected 恒为 false。而下面的 loadWindow/loadHeatmap 在缓存命中时会直接
  // 返回、不会回调 draw —— 于是重新进入本页时整页空白，一直等到下一次轮询（健康轮询
  // 5 秒）触发 onTick 里的 loadWindow(true) 才补画。
  draw(true);
  return host;
}

// firstPaint 为真时表示"本轮是 renderOverview 里的同步首绘"，节点尚未挂载；
// 其余调用（轮询回调）仍要求节点在文档里，避免离开页面后继续白画。
function draw(firstPaint = false) {
  if (!host) return;
  if (!firstPaint && !host.isConnected) return;
  const { store } = xtxRef;
  const metrics = state.snapshot;
  const points = state.series || [];
  const bucketSeconds = state.seriesBucket || pickBucketSeconds(state.hours);

  const head = h("div.page-head", {},
    h("div.page-title", {},
      h("h1", "概览"),
      h("p.sub", "本机 AMKR 的统一入口、实时速率与用量趋势。"),
    ),
    h("div.page-actions", {},
      state.snapshotAt ? freshness(state.snapshotAt) : null,
      store.health?.base_url
        ? buttonNode("复制入口地址", {
            variant: "secondary", small: true, iconName: "copy",
            onClick: () => copyText(store.health.base_url).then(() => toast("入口地址已复制")),
          })
        : null,
      segmented(TIME_RANGES.map((range) => ({ id: range.hours, label: range.short, title: range.label })),
        state.hours,
        (id) => { state.hours = Number(id); state.loading = true; draw(); loadWindow(true); },
        { "aria-label": "选择统计窗口" }),
    ),
  );

  const children = [head];

  if (store.connectionError) {
    children.push(notice(`无法连接 AMKR 服务：${store.connectionError}`, "error"));
  } else if (state.snapshotError && !metrics) {
    children.push(notice(`指标读取失败：${state.snapshotError}`, "error"));
  }
  if (store.health?.local_auth_enabled === false) {
    children.push(notice("本地鉴权未启用：管理接口对本机开放，建议在设置中启用。", "warn"));
  }

  if (!metrics) {
    children.push(skeleton("stats"));
    children.push(card(cardHead("流量趋势"), skeleton("chart")));
    render(host, children);
    return;
  }

  children.push(kpiTiles(metrics, points, bucketSeconds));
  // 所有看板卡放进同一个栅格，而不是每排一个 .grid-12：
  // 分开写时排内间距是 16px、排间却是 .content 的 24px，横向纵向对不上，
  // 整体节奏显得松散。合成一个栅格后，所有间隙统一为 gap。
  // 每排仍按最高卡等高（见 styles.css 的说明），因此排内底边平齐。
  children.push(h("div.grid-12", {},
    // 热力图紧贴 KPI 瓦片下方并占满整行：它是"一眼看节律"的图，
    // 放在页面末尾要滚到底才看得到；而 24 个小时列塞进 col-4 每格只剩十几像素。
    h("div.col-12", {}, heatmapCard()),
    h("div.col-8", {}, trendCard()),
    h("div.col-4", {}, unifiedCard()),
    h("div.col-4", {}, compositionCard(metrics)),
    h("div.col-4", {}, statusCard(metrics)),
    h("div.col-4", {}, waterfallCard(metrics)),
    h("div.col-4", {}, rankingCard("模型调用排行", metrics.models, "模型")),
    h("div.col-4", {}, rankingCard("调用方排行", metrics.caller_types, "调用方")),
    h("div.col-4", {}, rankingCard("上游 Token 排行", metrics.upstream_models, "上游模型的 Token", {
      value: (stats) => stats.total_tokens,
      tone: "secondary",
      format: (row) => `${formatCompact(row.value)} Token`,
    })),
    h("div.col-8", {}, tokenBreakdownCard(points, bucketSeconds)),
    h("div.col-4", {}, runtimeCard()),
  ));

  render(host, children);
}

// Token 构成随时间：堆叠柱，和折线看"总量趋势"互补，看"输入/输出结构变化"。
// 图例只作颜色索引、不带数字：窗口总量已经在 KPI 与 Token 构成卡上给过，
// 这里再算一遍（按图内桶求和）只会多出一个对不上的数字。
function tokenBreakdownCard(points, bucketSeconds) {
  return card(
    cardHead("Token 构成随时间",
      badge(rangeLabel(), "muted"),
      badge(`${bucketLabel(bucketSeconds)}/柱`, "muted"),
    ),
    legend([
      { label: "输入", tone: "primary" },
      { label: "输出", tone: "secondary" },
    ]),
    h("div", { style: { marginTop: "12px" } },
      stackedBars({ points, height: 220, bucketSeconds }),
    ),
  );
}
