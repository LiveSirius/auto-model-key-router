// 图表口径的纯函数层：不带 DOM，可被 node 直接单测。
//
// 这里集中处理"数据准不准"的三件事，UI 层只负责画：
//   1. 单位换算 —— 后端按 bucket_seconds 分桶计数，画速率必须换算成"每分钟"，
//      否则 5 分钟桶会把请求数显示成 5 倍的 RPM。
//   2. 加权聚合 —— 平均耗时/缓存率不能用各桶比率再平均，必须回到分子分母求和。
//   3. 缺口与残桶 —— 0 是真实读数（无请求），只有 null 才是缺失；最后一个桶
//      还在累加中，必须标出来，否则实时曲线尾端会被误读成流量骤降。

// 速率指标统一换算的分母：后端 current_rpm / current_tpm 就是 60 秒窗口计数。
export const RATE_SECONDS = 60;

// bucket_seconds 的取值阶梯（与后端 Query(ge=15, le=86400) 对齐）。
const BUCKET_LADDER = [15, 30, 60, 120, 180, 300, 600, 900, 1800, 3600, 7200, 10800, 21600, 43200, 86400];

// 后端 MAX_SERIES_POINTS = 500：超过会被 422 拒绝，所以取桶要留一格余量。
export const MAX_SERIES_POINTS = 500;

export const TIME_RANGES = [
  { hours: 1, label: "1 小时", short: "1h" },
  { hours: 6, label: "6 小时", short: "6h" },
  { hours: 24, label: "24 小时", short: "24h" },
  { hours: 72, label: "3 天", short: "3d" },
  { hours: 168, label: "7 天", short: "7d" },
];

// 选最小的、能满足点数上限的桶宽：越小越精确。
export function pickBucketSeconds(hours) {
  for (const candidate of BUCKET_LADDER) {
    if (Math.ceil((hours * 3600) / candidate) + 1 <= MAX_SERIES_POINTS) return candidate;
  }
  return BUCKET_LADDER[BUCKET_LADDER.length - 1];
}

export function bucketLabel(seconds) {
  if (seconds % 3600 === 0) return `${seconds / 3600} 小时`;
  if (seconds % 60 === 0) return `${seconds / 60} 分钟`;
  return `${seconds} 秒`;
}

// —— 指标目录 ——
// value(point, ctx) 返回该桶的读数；null/undefined 视为缺失。
// 每个指标显式声明单位与是否百分比，UI 才能选中正确的坐标轴刻度与 tooltip 格式。

export const METRICS = [
  {
    id: "rpm",
    label: "请求速率",
    short: "RPM",
    unit: "次/分",
    percent: false,
    hint: "每个时间桶的上游请求次数换算为每分钟",
    value: (point, ctx) => scale(point.requests, ctx.bucketSeconds),
    total: (sums) => scale(sums.requests, sums.hours * 3600),
    format: (value) => formatNumber(value, 1),
  },
  {
    id: "tpm",
    label: "Token 速率",
    short: "TPM",
    unit: "Token/分",
    percent: false,
    hint: "每个时间桶的 Token 用量换算为每分钟",
    value: (point, ctx) => scale(point.total_tokens, ctx.bucketSeconds),
    total: (sums) => scale(sums.total_tokens, sums.hours * 3600),
    format: (value) => formatCompactNumber(value),
  },
  {
    id: "success",
    label: "成功率",
    short: "成功率",
    unit: "%",
    percent: true,
    hint: "成功请求 / 全部请求",
    value: (point) => (point.requests ? point.successes / point.requests : null),
    total: (sums) => (sums.requests ? sums.successes / sums.requests : null),
    format: (value) => formatPercentValue(value),
  },
  {
    id: "cache",
    label: "缓存命中率",
    short: "缓存率",
    unit: "%",
    percent: true,
    hint: "缓存 Token / 输入 Token",
    value: (point) => (point.prompt_tokens ? point.cached_tokens / point.prompt_tokens : null),
    total: (sums) => (sums.prompt_tokens ? sums.cached_tokens / sums.prompt_tokens : null),
    format: (value) => formatPercentValue(value),
  },
  {
    id: "latency",
    label: "平均耗时",
    short: "耗时",
    unit: "ms",
    percent: false,
    hint: "按请求数加权的平均总耗时",
    value: (point) => (point.requests ? point.total_duration_ms / point.requests : null),
    total: (sums) => (sums.requests ? sums.total_duration_ms / sums.requests : null),
    format: (value) => formatDurationValue(value),
  },
  {
    id: "firstToken",
    label: "平均首字延迟",
    short: "首字",
    unit: "ms",
    percent: false,
    hint: "按请求数加权的平均首 Token 延迟",
    value: (point) => (point.requests ? point.total_first_token_ms / point.requests : null),
    total: (sums) => (sums.requests ? sums.total_first_token_ms / sums.requests : null),
    format: (value) => formatDurationValue(value),
  },
  {
    id: "tokens",
    label: "Token 用量",
    short: "Token",
    unit: "Token",
    percent: false,
    hint: "每个时间桶的 Token 总量（不换算速率）",
    value: (point) => point.total_tokens,
    total: (sums) => sums.total_tokens,
    format: (value) => formatCompactNumber(value),
  },
];

export const METRIC_MAP = Object.fromEntries(METRICS.map((metric) => [metric.id, metric]));

// count 是"每 bucketSeconds 秒的次数"，换算成每分钟。
// 用 60000/bucketSeconds 的整数化写法避免浮点尾差：bucket=60 → ×1。
function scale(count, bucketSeconds) {
  if (count === null || count === undefined) return null;
  if (!bucketSeconds) return Number(count);
  return (Number(count) * RATE_SECONDS) / bucketSeconds;
}

export function metricValue(point, metric, bucketSeconds) {
  return metric.value(point, { bucketSeconds });
}

// 窗口级汇总：把所有桶的分子分母分别相加后再求比率。
// 直接对每桶比率取平均会被小样本桶带偏（1 次请求失败 = 0%，权重却和 1000 次相同）。
export function windowSums(points) {
  const sums = {
    requests: 0, successes: 0, failures: 0, retries: 0,
    prompt_tokens: 0, completion_tokens: 0, total_tokens: 0, cached_tokens: 0,
    total_duration_ms: 0, total_first_token_ms: 0,
    hours: 0, buckets: points.length, zeroBuckets: 0,
  };
  for (const point of points || []) {
    sums.requests += number(point.requests);
    sums.successes += number(point.successes);
    sums.failures += number(point.failures);
    sums.retries += number(point.retries);
    sums.prompt_tokens += number(point.prompt_tokens);
    sums.completion_tokens += number(point.completion_tokens);
    sums.total_tokens += number(point.total_tokens);
    sums.cached_tokens += number(point.cached_tokens);
    sums.total_duration_ms += number(point.total_duration_ms);
    sums.total_first_token_ms += number(point.total_first_token_ms);
    if (!number(point.requests)) sums.zeroBuckets += 1;
  }
  return sums;
}

// 曲线的数据点：{ index, at, value, partial }
// partial 标记"仍在累加中的尾桶"，图表用虚线收尾而不是把缺口画成真实下跌。
export function series(points, metric, bucketSeconds) {
  return (points || []).map((point, index) => ({
    index,
    at: point.started_at,
    endedAt: point.ended_at,
    value: metricValue(point, metric, bucketSeconds),
    partial: point.complete === false,
    raw: point,
  }));
}

// 把数据收窄到最后一个完整的桶：用于"已完结区间"的汇总与对比。
export function completeOnly(points) {
  return (points || []).filter((point) => point.complete !== false);
}

// 一个系列点是否有真实读数：null / undefined / NaN 都算缺失。
//
// 均值型指标（耗时、首字延迟）在无请求的桶上返回 null 而不是 0 ——
// "0 次请求的平均耗时"没有意义。所以"缺口"与"空闲（0 次/分）"是两件事：
// 前者没有读数，后者是有读数的 0。判定逻辑集中在这里，避免图表各处各写一遍。
export function readable(point) {
  const value = point?.value;
  return value !== null && value !== undefined && Number.isFinite(value);
}

// 缺口桥接：相邻两个可读点之间隔了缺失的桶时，给出这对下标。
//
// 为什么需要：稀疏流量下（15 秒一桶、偶尔才有请求）耗时曲线会被缺口切成几十段
// 碎片，看上去像"图表坏了"而不是"这段没有请求"。图表用虚线跨越缺口连接，
// 折线因此读起来是连续的，而虚线又说明这一段没有采样、不是实测值。
export function gapBridges(seriesPoints) {
  const out = [];
  let previous = -1;
  (seriesPoints || []).forEach((point, index) => {
    if (!readable(point)) return;
    // 首尾的缺口没有可连的另一端，前面的 previous < 0 与末尾不再有可读点
    // 都自然落在条件之外：只桥接"两侧都有读数"的缺口。
    if (previous >= 0 && index > previous + 1) out.push({ from: previous, to: index });
    previous = index;
  });
  return out;
}

// —— 坐标轴刻度 ——
// 标准 1/2/2.5/5/10 阶梯，保证刻度值是"人能读"的整数而不是数据最大值。
export function niceStep(rough) {
  if (!(rough > 0) || !Number.isFinite(rough)) return 1;
  const exponent = Math.floor(Math.log10(rough));
  const magnitude = 10 ** exponent;
  const normalized = rough / magnitude;
  const factor = normalized <= 1 ? 1 : normalized <= 2 ? 2 : normalized <= 2.5 ? 2.5 : normalized <= 5 ? 5 : 10;
  return factor * magnitude;
}

// 有数据时轴顶必须 >= 最大值，否则曲线会被裁掉。
export function axisScale(values, { percent = false, ticks: tickCount = 4, min: minValue = 0 } = {}) {
  if (percent) return { min: 0, max: 100, step: 25, ticks: [0, 25, 50, 75, 100] };
  const finite = (values || []).filter((value) => Number.isFinite(value));
  const dataMax = finite.length ? Math.max(...finite) : 0;
  const dataMin = finite.length ? Math.min(...finite) : 0;
  const floor = Math.min(minValue, dataMin);
  if (!(dataMax > floor)) {
    // 全零或全等值：给一条非零轴，避免曲线贴底或除以 0。
    const top = dataMax > 0 ? dataMax : 1;
    const step = niceStep(top / tickCount);
    return buildScale(floor, Math.ceil(top / step) * step || step, step);
  }
  const step = niceStep((dataMax - floor) / tickCount);
  const max = Math.ceil(dataMax / step) * step;
  return buildScale(floor, max, step);
}

function buildScale(min, max, step) {
  const ticks = [];
  // 用乘法定位刻度，避免 step 是小数时反复累加产生 0.30000000000000004 这类值。
  const count = Math.round((max - min) / step);
  for (let i = 0; i <= count; i += 1) ticks.push(roundTo(min + i * step, step));
  return { min, max, step, ticks };
}

function roundTo(value, step) {
  const decimals = Math.max(0, -Math.floor(Math.log10(step)) + 1);
  return Number(value.toFixed(Math.min(decimals, 10)));
}

// 时间轴标签：按可用像素选间隔，保证相邻标签不会重叠。
// 入参既可能是原始点（started_at）也可能是 series() 的产物（at），两者都要认。
export function timeTicks(points, maxLabels = 6) {
  const list = points || [];
  if (!list.length) return [];
  const at = (point) => point?.at ?? point?.started_at;
  const stride = Math.max(1, Math.ceil(list.length / Math.max(1, maxLabels)));
  const out = [];
  for (let i = 0; i < list.length; i += stride) out.push({ index: i, at: at(list[i]) });
  // 末尾始终补一个，读数才知道曲线右端对应几点。
  const last = list.length - 1;
  if (out[out.length - 1]?.index !== last) {
    if (last - (out[out.length - 1]?.index ?? -99) < stride / 2) out.pop();
    out.push({ index: last, at: at(list[last]) });
  }
  return out;
}

// 从后往前取最新一个"有读数"的值：全零窗口里最新读数就是 0，不是"—"。
export function latestValue(seriesPoints) {
  for (let i = seriesPoints.length - 1; i >= 0; i -= 1) {
    if (seriesPoints[i].value !== null && seriesPoints[i].value !== undefined) return seriesPoints[i];
  }
  return null;
}

// 环比：前半段 vs 后半段，用于给 KPI 一个方向感。
// 前半段为 0 时返回 null（"从 0 增长"没有百分比意义），而不是 Infinity。
export function trend(seriesPoints, reducer = "sum") {
  const values = seriesPoints.map((point) => point.value).filter((value) => Number.isFinite(value));
  if (values.length < 4) return null;
  const mid = Math.floor(values.length / 2);
  const before = values.slice(0, mid);
  const after = values.slice(mid);
  const reduce = reducer === "mean"
    ? (list) => (list.length ? list.reduce((a, b) => a + b, 0) / list.length : 0)
    : (list) => list.reduce((a, b) => a + b, 0);
  const previous = reduce(before);
  const current = reduce(after);
  if (!previous) return null;
  return { previous, current, change: (current - previous) / previous };
}

// 堆叠构成：输入 / 输出 Token。两者相加应等于 total_tokens（后端口径）。
export function tokenComposition(sums) {
  return [
    { id: "prompt", label: "输入 Token", value: sums.prompt_tokens, tone: "primary" },
    { id: "completion", label: "输出 Token", value: sums.completion_tokens, tone: "secondary" },
  ];
}

// 排行榜：把 { name: stats } 或嵌套两层压成有序数组。
export function rank(entries, { value = (stats) => number(stats.requests), limit = 6 } = {}) {
  return Object.entries(entries || {})
    .map(([name, stats]) => ({ name, stats, value: value(stats) }))
    .filter((row) => row.value > 0)
    .sort((a, b) => b.value - a.value)
    .slice(0, limit);
}

// 状态码分桶：2xx 成功 / 4xx 客户端 / 5xx 服务端 / 其它。
// 颜色语义在 UI 层统一，这里只负责分类。
export function statusGroups(statusCodes) {
  const groups = { ok: 0, client: 0, server: 0, other: 0 };
  for (const [code, count] of Object.entries(statusCodes || {})) {
    const value = Number(code);
    const total = number(count);
    if (value >= 200 && value < 300) groups.ok += total;
    else if (value >= 400 && value < 500) groups.client += total;
    else if (value >= 500) groups.server += total;
    else groups.other += total;
  }
  return groups;
}

// —— 热力图：星期 × 半小时的用量矩阵 ——
// 数据来自 /metrics/series?hours=168&bucket_seconds=1800：半小时一个桶，每个桶的
// started_at 本身就直接落在某个 (星期, 时段) 格子里，所以不需要客户端二次聚合，
// 把桶放进格子求和即可。这也是唯一不需要改后端的做法。

export const WEEKDAY_LABELS = ["周一", "周二", "周三", "周四", "周五", "周六", "周日"];
export const HEATMAP_DAYS = 7;
// 一格半小时：7 × 48 = 336 格。整点桶会把"10:00 起的峰"和"10:30 起的峰"摊进同一格，
// 半小时粒度才分得开（后端 bucket_seconds 下限 15 秒，1800 完全在范围内）。
export const HEATMAP_SLOT_MINUTES = 30;
export const HEATMAP_SLOTS = (24 * 60) / HEATMAP_SLOT_MINUTES;
// 4 档 + 0 档。档数再多，浅色系里相邻两档的肉眼差异就没了。
export const HEAT_LEVELS = 4;

// 热力图的两个口径：直接取桶内原始计数，不做速率换算 —— 热力图回答的是
// "这半小时发生了多少"，换算成每分钟只会把 336 格读数全除以 30。
export const HEATMAP_METRICS = [
  {
    id: "requests",
    label: "请求数量",
    pick: (point) => number(point.requests),
    format: (value) => `${formatCompactNumber(value)} 次`,
  },
  {
    id: "tokens",
    label: "Token 数量",
    pick: (point) => number(point.total_tokens),
    format: (value) => `${formatCompactNumber(value)} Token`,
  },
];

export const HEATMAP_METRIC_MAP = Object.fromEntries(
  HEATMAP_METRICS.map((metric) => [metric.id, metric]));

// 按 Asia/Shanghai 取"星期几 + 第几个半小时"。不能读 Date 的 getDay/getHours：后端
// 时间戳都带 +08:00，但浏览器时区可能不同，本地字段会把整张矩阵平移。
const BEIJING_PARTS = new Intl.DateTimeFormat("zh-CN", {
  timeZone: "Asia/Shanghai",
  year: "numeric", month: "2-digit", day: "2-digit",
  hour: "2-digit", minute: "2-digit", hourCycle: "h23",
});

export function beijingSlot(value) {
  const date = value instanceof Date ? value : new Date(value);
  if (Number.isNaN(date.getTime())) return null;
  const parts = {};
  for (const part of BEIJING_PARTS.formatToParts(date)) parts[part.type] = part.value;
  const year = Number(parts.year);
  const month = Number(parts.month);
  const day = Number(parts.day);
  const hour = Number(parts.hour);
  const minute = Number(parts.minute);
  if (![year, month, day, hour, minute].every(Number.isFinite)) return null;
  // 用 UTC 构造"北京那一天的零点"再取 UTC 星期，绕开浏览器本地时区对 Date 的影响；
  // getUTCDay() 的 0 是周日，映射成 0=周一。
  const weekday = (new Date(Date.UTC(year, month - 1, day)).getUTCDay() + 6) % 7;
  // 向下取整到半小时：10:30 属于第 21 格，不能被并进 10:00 那一格。
  const slot = Math.floor((hour * 60 + minute) / HEATMAP_SLOT_MINUTES);
  return { weekday, slot };
}

// 时段标签（"09:30"）：表头与读数气泡共用一份格式，免得两处各写一遍补零。
export function slotLabel(slot) {
  const minutes = slot * HEATMAP_SLOT_MINUTES;
  const hour = String(Math.floor(minutes / 60)).padStart(2, "0");
  return `${hour}:${String(minutes % 60).padStart(2, "0")}`;
}

// 每格记 { value, buckets, partial }：buckets 用来区分"没数据"与"确实是 0"，
// 前者是窗口没覆盖到、后者是这半小时真的没有流量，画成同一种灰会谎报。
export function heatmapCells(points, { value = (point) => number(point.requests) } = {}) {
  const cells = Array.from({ length: HEATMAP_DAYS }, () =>
    Array.from({ length: HEATMAP_SLOTS }, () => ({ value: 0, buckets: 0, partial: false })));
  for (const point of points || []) {
    const slot = beijingSlot(point?.started_at);
    if (!slot) continue;
    const cell = cells[slot.weekday][slot.slot];
    cell.value += value(point);
    cell.buckets += 1;
    // 累加中的尾桶天然偏低，格子要标出来，否则最新一格会被读成"突然没流量"。
    if (point.complete === false) cell.partial = true;
  }
  return cells;
}

// 色阶上限取有数据格子的最大值：热力图要看的是"什么时候是峰值"，按分位数归一
// 会把所有格子摊平、反而看不出峰。
export function heatmapScale(cells) {
  const values = (cells || []).flat()
    .filter((cell) => cell.buckets > 0)
    .map((cell) => cell.value);
  return values.length ? Math.max(...values) : 0;
}

// 0 单独一档（真·空闲），其余按最大值线性切 steps 档。没有数据时返回 0。
export function heatmapLevel(value, max, steps = 4) {
  if (!(max > 0)) return 0;
  return Math.min(steps, Math.max(0, Math.ceil(value / (max / steps))));
}

// —— 数字格式化（纯函数，与 dom.js 的展示版保持一致精度）——

export function formatNumber(value, digits = 1) {
  if (!Number.isFinite(value)) return "-";
  if (Math.abs(value) >= 1000) return formatCompactNumber(value);
  if (Number.isInteger(value)) return String(value);
  return value.toFixed(digits).replace(/\.0+$/, "");
}

export function formatCompactNumber(value) {
  if (!Number.isFinite(value)) return "-";
  const abs = Math.abs(value);
  if (abs >= 1e9) return `${trimZero(value / 1e9)}B`;
  if (abs >= 1e6) return `${trimZero(value / 1e6)}M`;
  if (abs >= 1e3) return `${trimZero(value / 1e3)}K`;
  if (Number.isInteger(value)) return String(value);
  return trimZero(value);
}

export function formatPercentValue(ratio, digits = 1) {
  if (!Number.isFinite(ratio)) return "-";
  const percent = ratio * 100;
  // 0.5% 以下不四舍五入成 0%，否则"几乎全失败"会看起来像"没有缓存"。
  if (percent > 0 && percent < 0.5) return "<0.5%";
  if (percent > 99.5 && percent < 100) return ">99.5%";
  return `${percent.toFixed(digits)}%`;
}

export function formatDurationValue(ms) {
  if (!Number.isFinite(ms)) return "-";
  if (ms >= 10000) return `${(ms / 1000).toFixed(1)}s`;
  if (ms >= 1000) return `${(ms / 1000).toFixed(2)}s`;
  return `${Math.round(ms)}ms`;
}

function trimZero(value) {
  return value.toFixed(1).replace(/\.0$/, "");
}

function number(value) {
  const n = Number(value);
  return Number.isFinite(n) ? n : 0;
}
