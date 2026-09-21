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

// 实时监控的快捷窗口（概览页）。刻意只到 7 天：这一页看的是"现在怎么样"，
// 再长的窗口该去用量统计页看。
export const TIME_RANGES = [
  { hours: 1, label: "1 小时", short: "1h" },
  { hours: 6, label: "6 小时", short: "6h" },
  { hours: 24, label: "24 小时", short: "24h" },
  { hours: 72, label: "3 天", short: "3d" },
  { hours: 168, label: "7 天", short: "7d" },
];

// 历史用量窗口（用量统计页）：在实时窗口之上加月/季/半年/年，外加「全部」。
// 上限 8760 小时（1 年）来自后端 /metrics/series 的 hours 上界。
export const LONG_RANGES = [
  { hours: 720, label: "1 个月", short: "1m" },
  { hours: 2160, label: "3 个月", short: "3m" },
  { hours: 4320, label: "6 个月", short: "6m" },
  { hours: 8760, label: "1 年", short: "1y" },
];

// ALL_HISTORY 是「全部」这个虚拟窗口的 id。
//
// 它不能用固定 hours 表达：历史有多长只有服务端知道。因此先用
// /metrics/requests?all_history=true 读回 earliest（window.from 为 null 时说明库是空的），
// 再据此推导 hours —— 这条路由的 hours 上限是 720，所以**不能**用 hours 表达「全部」，
// 但它的 all_history 分支会把 window.from 设成库里的最早时间，正好是我们要的跨度。
// 推导结果仍受 series 的 8760 上限约束，超出时按 8760 截断并在界面上标注。
export const ALL_HISTORY = "all";
export const MAX_HISTORY_HOURS = 8760;
export const USAGE_RANGES = [
  ...TIME_RANGES,
  ...LONG_RANGES,
  { hours: ALL_HISTORY, label: "全部历史", short: "全部" },
];

// 窗口描述：把选中项解析成"要请求多少小时、界面上怎么称呼"。
// selection 既可以是小时数，也可以是 ALL_HISTORY；「全部」的真实跨度由调用方用
// historyHours() 从服务端读回后经 allHours 传入（拿不到时退回上限）。
export function rangeSpec(selection, { allHours = null } = {}) {
  if (selection === ALL_HISTORY) {
    const hours = allHours || MAX_HISTORY_HOURS;
    return {
      hours,
      label: "全部历史",
      short: "全部",
      all: true,
      // 跨度顶到后端上限时必须说明"只覆盖到上限"，否则"全部"会被当成真的是全部。
      truncated: hours >= MAX_HISTORY_HOURS,
    };
  }
  const found = USAGE_RANGES.find((range) => range.hours === selection);
  const hours = Number(selection) || 1;
  return {
    hours,
    label: found?.label || `${hours} 小时`,
    short: found?.short || `${hours}h`,
    all: false,
    truncated: false,
  };
}

// 从服务端给出的最早时间推导历史跨度（小时），并夹到后端上限内。
// 返回 null 表示"库里没有任何记录"，此时没有窗口可画。
export function historyHours(earliest, now = Date.now()) {
  if (!earliest) return null;
  const started = Date.parse(earliest);
  if (!Number.isFinite(started)) return null;
  const elapsed = now - started;
  if (!(elapsed > 0)) return null;
  // 向上取整：跨度是 1.2 小时时取 2 小时，否则会漏掉最早那条记录所在的部分桶。
  return Math.min(MAX_HISTORY_HOURS, Math.max(1, Math.ceil(elapsed / 3600000)));
}

// 选最小的、能满足点数上限的桶宽：越小越精确。
export function pickBucketSeconds(hours) {
  for (const candidate of BUCKET_LADDER) {
    if (Math.ceil((hours * 3600) / candidate) + 1 <= MAX_SERIES_POINTS) return candidate;
  }
  return BUCKET_LADDER[BUCKET_LADDER.length - 1];
}

export function bucketLabel(seconds) {
  // 天级桶说"1 天"而不是"24 小时"：长窗口的读数区会写"366 个数据点"，配"24 小时/点"
  // 要心算才知道覆盖多久，而"1 天/点"一眼就懂。
  if (seconds % 86400 === 0) return `${seconds / 86400} 天`;
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
  // 计数型（不换算速率）：累计曲线要用"一共多少次"，而不是"每分钟多少次"。
  // 与 rpm 的分工是单位不同，不是同一指标的两种画法。
  {
    id: "requests",
    label: "请求数",
    short: "请求",
    unit: "次",
    percent: false,
    hint: "每个时间桶的上游请求次数（不换算速率）",
    value: (point) => point.requests,
    total: (sums) => sums.requests,
    format: (value) => formatNumber(value, 0),
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

// —— 历史用量聚合 ——
// 长窗口（月/季/年）里逐桶列表没有可读性：366 个日桶要一行行看。
// 历史视角要的是"按天/按时段的汇总"，所以下面把这三种聚合收敛成纯函数。

// 北京日历日（"YYYY-MM-DD"）。用 en-CA 是因为它的短日期格式恰好是 ISO 顺序，
// 不必手工拼接补零（拼接版本容易在月份/日期上写反）。
const BEIJING_DATE = new Intl.DateTimeFormat("en-CA", {
  timeZone: "Asia/Shanghai", year: "numeric", month: "2-digit", day: "2-digit",
});

export function beijingDate(value) {
  const date = value instanceof Date ? value : new Date(value);
  if (Number.isNaN(date.getTime())) return null;
  return BEIJING_DATE.format(date);
}

// 按北京日历日聚合序列点。
//
// 分母型字段（耗时、首字）必须**加权求和后再除**，不能对每日均值再平均 ——
// 与 windowSums 同一个理由：1 次请求的一天和 1000 次请求的一天权重相同会算错。
export function dailyUsage(points) {
  const days = new Map();
  for (const point of points || []) {
    const key = beijingDate(point?.started_at);
    if (!key) continue;
    let day = days.get(key);
    if (!day) {
      day = {
        date: key, requests: 0, successes: 0, failures: 0, retries: 0,
        prompt_tokens: 0, completion_tokens: 0, total_tokens: 0, cached_tokens: 0,
        total_duration_ms: 0, total_first_token_ms: 0, buckets: 0, partial: false,
      };
      days.set(key, day);
    }
    day.requests += number(point.requests);
    day.successes += number(point.successes);
    day.failures += number(point.failures);
    day.retries += number(point.retries);
    day.prompt_tokens += number(point.prompt_tokens);
    day.completion_tokens += number(point.completion_tokens);
    day.total_tokens += number(point.total_tokens);
    day.cached_tokens += number(point.cached_tokens);
    day.total_duration_ms += number(point.total_duration_ms);
    day.total_first_token_ms += number(point.total_first_token_ms);
    day.buckets += 1;
    if (point.complete === false) day.partial = true;
  }
  // 字典序即时间序（ISO 日期），不必解析回 Date 再比。
  return [...days.values()].sort((a, b) => a.date.localeCompare(b.date));
}

// 按时段（0-23 时，北京时间）聚合：回答"高峰在几点"。
//
// 与热力图的分工：热力图看**周内**节律（工作日 vs 周末），这里把整段窗口压成
// 24 根柱子看**日内**节律 —— 3 个月以上的窗口用热力图会糊成一团，看时段分布更实用。
export function hourlyProfile(points, { value = (point) => number(point.requests) } = {}) {
  const hours = Array.from({ length: 24 }, (_, hour) => ({ hour, value: 0, buckets: 0 }));
  for (const point of points || []) {
    const slot = beijingSlot(point?.started_at);
    if (!slot) continue;
    // beijingSlot 的 slot 是半小时格，除以 2 得到小时。它同时保证了时区正确。
    const hour = Math.floor(slot.slot / 2);
    hours[hour].value += value(point);
    hours[hour].buckets += 1;
  }
  return hours;
}

// —— 桑基流向布局 ——
//
// 与服务端 /ui/workspace-usage.json 的 links 对应：每条连边给定
// { source_layer, target_layer, source, target, requests, total_tokens }。
// 这里只做"算"，"画"在 charts.js，方便单测。
//
// 布局算法是分层的：每层节点按流入/流出的总量分配高度，再按顺序堆叠；
// 连边的纵向位置由"该层已用掉的量"决定，因此同一节点的多条出边依次排开、
// 不会重叠。不做迭代优化（真正的桑基图会用松弛法减少交叉）——层数固定为 5、
// 节点数在几十个量级，顺序堆叠已经可读，而迭代会让渲染变得不确定、也无法单测。

// sankeyLayers 从连边里归纳出每层的节点与各自的总量。
//
// 节点的量取 **max(流出合计, 流入合计)**，不是"所有连边里的最大值"：一个节点
// 的多条出边要依次排在它身上，量必须是这些出边之和，否则连边会溢出节点（例如
// 3+1 的流出配上一个 3 的节点高度，第二条流带就会画到节点外面）。末层只有流入、
// 首层只有流出，取 max 让中间层两侧都够用。
export function sankeyLayers(links) {
  const layers = [];
  const ensure = (index) => {
    while (layers.length <= index) layers.push(new Map());
    return layers[index];
  };
  const bump = (map, name, field, value) => {
    const entry = map.get(name) || { in: 0, out: 0 };
    entry[field] += value;
    map.set(name, entry);
  };
  for (const link of links || []) {
    const value = number(link.requests);
    bump(ensure(number(link.source_layer)), link.source, "out", value);
    bump(ensure(number(link.target_layer)), link.target, "in", value);
  }
  return layers.map((nodes) => {
    const items = [...nodes.entries()].map(([name, flow]) => ({
      name,
      value: Math.max(flow.in, flow.out),
    }));
    // 同层内按量降序：大的节点在上方，读起来更稳，也让"主要流向"落在视线高度。
    // 同量时按名字排序，保证同样的数据每次渲染顺序一致（顺序变了图就跳）。
    items.sort((a, b) => (b.value - a.value) || a.name.localeCompare(b.name, "zh-CN"));
    return items;
  });
}

// sankeyLayout 把连边排成可直接绘制的坐标。
//
// 返回：
//   columns: [{ x, width, nodes: [{name, value, y, height, offset}] }]
//   edges:   [{ 路径用的 source/target 锚点, value, thickness, link }]
//
// **纵向只用一把尺子**（全局 scale），这是关键：若每层各自缩放到满高，同一节点的
// 入边按来源层的比例算、出边按本层比例算，两者会得到不同厚度，流带就会溢出节点。
// 用同一个 scale 后，节点高度与连边厚度都是"绝对请求数 × scale"，两端天然吻合。
//
// 代价是层与层之间总量不等时（例如 provider_id 为空的请求在中间断掉），层不会
// 占满高度——这正是想要的：那段留白就是"在此处丢失的流量"。层内做垂直居中，
// 让留白分在上下两侧而不是全堆在底部。
export function sankeyLayout(links, { width = 900, height = 420, nodeWidth = 14, gap = 12 } = {}) {
  const columns = sankeyLayers(links);
  const count = columns.length;
  if (!count) return { columns: [], edges: [] };

  const totals = columns.map((nodes) => nodes.reduce((sum, node) => sum + node.value, 0));
  const maxTotal = Math.max(...totals, 0);
  // 最大层决定纵向尺度：它必须连同自身间隙一起装进可用高度。
  const maxGaps = Math.max(0, Math.max(...columns.map((nodes) => nodes.length)) - 1);
  const scale = maxTotal > 0 ? Math.max(0, height - gap * maxGaps) / maxTotal : 0;

  const span = Math.max(1, width - nodeWidth);
  const step = count > 1 ? span / (count - 1) : 0;

  const placed = columns.map((nodes, index) => {
    const columnHeight = totals[index] * scale + gap * Math.max(0, nodes.length - 1);
    // 垂直居中：留白平分到上下，而不是全压在底部。
    let cursor = Math.max(0, (height - columnHeight) / 2);
    const x = index * step;
    return {
      x,
      width: nodeWidth,
      // 节点的 x 必须挂在**节点自己**身上：连边锚点要同时用到两端节点的 x
      // （左边缘进、右边缘出），只有列上带 x 的话锚点会算成 NaN/undefined，
      // 整条流带的路径就退化成 "M NaN … L undefined … Z" 而被浏览器丢弃。
      nodes: nodes.map((node) => {
        const nodeHeight = node.value * scale;
        const entry = { ...node, x, y: cursor, height: nodeHeight, offset: 0, outOffset: 0 };
        cursor += nodeHeight + gap;
        return entry;
      }),
    };
  });

  // 建立 name -> 节点 的索引。**只在层内唯一**：同一个名字（例如 gpt-4o 既是
  // 请求模型又是实际模型）会出现在不同层，因此键必须带上层号。
  const index = new Map();
  placed.forEach((column, layer) => {
    for (const node of column.nodes) index.set(`${layer}:${node.name}`, node);
  });

  const edges = [];
  for (const link of links || []) {
    const sourceLayer = number(link.source_layer);
    const targetLayer = number(link.target_layer);
    const source = index.get(`${sourceLayer}:${link.source}`);
    const target = index.get(`${targetLayer}:${link.target}`);
    if (!source || !target) continue;
    const value = number(link.requests);
    // 与节点同一个 scale：两端厚度因此必然与各自节点的高度体系一致。
    const thickness = value * scale;

    const edge = {
      link,
      value,
      thickness,
      source: { x: source.x + nodeWidth, y: source.y + source.outOffset },
      target: { x: target.x, y: target.y + target.offset },
      sourceName: link.source,
      targetName: link.target,
      sourceLayer,
      targetLayer,
    };
    source.outOffset += thickness;
    target.offset += thickness;
    edges.push(edge);
  }

  return { columns: placed, edges, width, height, nodeWidth, scale };
}

// sankeyLinkPath 生成一条连边的闭合路径（流带）。
//
// 上下两边各是一条三次贝塞尔，控制点取水平跨度的 40%：这是桑基图的惯例，让流带
// 在中段自然收束；用直线会让密集的图看起来像一团交叉的线段。
//
// 返回字符串而不是对象：调用方只用到路径本身。
export function sankeyLinkPath(edge) {
  const { source, target } = edge;
  const control = (target.x - source.x) * 0.4;
  const top = `M ${source.x} ${source.y} C ${source.x + control} ${source.y}, ${target.x - control} ${target.y}, ${target.x} ${target.y}`;
  const back = `L ${target.x} ${target.y + edge.thickness} C ${target.x - control} ${target.y + edge.thickness}, ${source.x + control} ${source.y + edge.thickness}, ${source.x} ${source.y + edge.thickness} Z`;
  return `${top} ${back}`;
}

// 轴/节点标签的**排版估算**放在这里（而不是 charts.js）：charts.js 碰 DOM，探针
// 加载不了，截断长度算错就只能靠肉眼在浏览器里发现——桑基图流带坐标缺失那个 bug
// 正是这样漏出去的。
//
// 10px 字号下的近似字宽：CJK/全角按 10px，ASCII 按 5.6px。混排时不能只按字符数
// 估：'gpt-4o-2024-08-06'（17 字）与 17 个汉字宽度差近一倍。
export function textWidth(text, size = 10) {
  let units = 0;
  for (const ch of String(text ?? "")) units += ch.codePointAt(0) > 0x2e80 ? 1 : 0.56;
  return units * size;
}

// 把标签截断到能塞进 budget 像素，放得下就原样返回。
//
// 用二分找最长前缀而不是"按字数换算"：混排文本每多一个字宽度增量不同，累加判断
// 才对得上 textWidth；省略号本身也占位，所以先给结尾留一个全角位。
export function fitLabel(name, budget, size = 10) {
  const text = String(name ?? "");
  if (textWidth(text, size) <= budget) return text;
  const chars = [...text];
  const room = budget - size;
  let low = 0;
  let high = chars.length;
  while (low < high) {
    const mid = Math.ceil((low + high) / 2);
    if (textWidth(chars.slice(0, mid).join(""), size) <= room) low = mid;
    else high = mid - 1;
  }
  return `${chars.slice(0, low).join("")}…`;
}

// 节点标签能占多宽：标签写在列与列之间的走廊里，而**最右那条走廊挤着两个标签**
// （倒数第二列从左往右写、末列从右往左写），所以末列只能分到走廊的一半。
// 列宽与左右各 6px 的偏移要从走廊里扣掉。
export function sankeyLabelBudget({ step, nodeWidth = 14, last = false, inset = 6 } = {}) {
  const room = Math.max(20, (step || 0) - nodeWidth - inset * 2);
  return last ? room / 2 : room;
}

export function cumulativeSeries(seriesPoints) {
  let running = 0;
  return (seriesPoints || []).map((point) => {
    if (readable(point)) running += point.value;
    return { ...point, value: running };
  });
}

// 分段对比：把窗口按时间对半切开，比较前后两半。
//
// 用"对半"而不是"与上一个等长窗口比"：后者要为每种窗口再多取一份数据，
// 而长窗口（1 年）翻倍查询代价很高。对半切只用手上已有的点，且读作
// "后半段相对前半段"同样能回答"用量在涨还是在跌"。
export function splitCompare(points, reducer = "sum") {
  const list = completeOnly(points);
  if (list.length < 2) return null;
  const half = Math.floor(list.length / 2);
  const before = summarize(list.slice(0, half), reducer);
  const after = summarize(list.slice(half), reducer);
  if (before === null || after === null) return null;
  const change = before ? (after - before) / before : null;
  return { before, after, change };
}

function summarize(seriesPoints, reducer) {
  const values = (seriesPoints || []).filter(readable).map((point) => point.value);
  if (!values.length) return null;
  if (reducer === "avg") return values.reduce((sum, value) => sum + value, 0) / values.length;
  return values.reduce((sum, value) => sum + value, 0);
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
