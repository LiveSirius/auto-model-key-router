// 图表口径回归探针：直接驱动 webui/chart-math.js 的纯函数，锁住"数据准不准"。
// 用法：node webui_chart_probe.mjs，结果以 JSON 打到 stdout，失败时退出码 1。
//
// 为什么用 node 而不是浏览器：这些函数不带 DOM，node 里跑得最快；而它们算错的
// 后果（速率没换算、平均值二次平均、残桶当成真实下跌）都只在数字上体现，
// 用真实模块断言比在浏览器里"看着对"可靠。

import { pathToFileURL } from "node:url";
import path from "node:path";

const MODULE = path.resolve(
  import.meta.dirname,
  "../auto_model_key_router/webui/chart-math.js",
);

const m = await import(pathToFileURL(MODULE).href);

const checks = {};
const near = (a, b, epsilon = 1e-9) =>
  Number.isFinite(a) && Number.isFinite(b) && Math.abs(a - b) < epsilon;
const check = (name, ok, detail = "") => {
  checks[name] = ok ? true : `FAILED${detail ? `: ${detail}` : ""}`;
};

// —— 桶宽选择：必须满足后端 MAX_SERIES_POINTS，且取阶梯里最小的（越小越精确）——
const LADDER = [15, 30, 60, 120, 180, 300, 600, 900, 1800, 3600, 7200, 10800, 21600, 43200, 86400];
check("bucket_1h_is_finest", m.pickBucketSeconds(1) === 15, String(m.pickBucketSeconds(1)));
check("bucket_24h_is_180s", m.pickBucketSeconds(24) === 180, String(m.pickBucketSeconds(24)));
check("bucket_168h_is_1800s", m.pickBucketSeconds(168) === 1800, String(m.pickBucketSeconds(168)));
for (const hours of [1, 6, 24, 72, 168, 720]) {
  const bucket = m.pickBucketSeconds(hours);
  const points = Math.ceil((hours * 3600) / bucket) + 1;
  check(`bucket_points_within_cap_${hours}h`, points <= m.MAX_SERIES_POINTS, `${points} > ${m.MAX_SERIES_POINTS}`);
  // 必须是"刚好够用"的最小桶：上一个更小的阶梯要么不存在，要么会超上限。
  const index = LADDER.indexOf(bucket);
  check(`bucket_is_smallest_viable_${hours}h`, index >= 0, `bucket ${bucket} 不在阶梯里`);
  if (index > 0) {
    const smaller = LADDER[index - 1];
    check(
      `bucket_smaller_would_overflow_${hours}h`,
      Math.ceil((hours * 3600) / smaller) + 1 > m.MAX_SERIES_POINTS,
      `${smaller}s 也未超上限，选桶偏粗`,
    );
  }
}

// —— 速率换算：桶宽不是 60 秒时必须折算成每分钟 ——
const rpm = m.METRIC_MAP.rpm;
check("rpm_60s_is_identity", near(m.metricValue({ requests: 30 }, rpm, 60), 30));
check("rpm_15s_scales_up", near(m.metricValue({ requests: 3 }, rpm, 15), 12));
check("rpm_300s_scales_down", near(m.metricValue({ requests: 500 }, rpm, 300), 100));

const tpm = m.METRIC_MAP.tpm;
check("tpm_180s_scales", near(m.metricValue({ total_tokens: 9000 }, tpm, 180), 3000));

// 比率型指标与桶宽无关，不能被速率换算污染。
const cache = m.METRIC_MAP.cache;
check("cache_rate_ignores_bucket", near(m.metricValue({ prompt_tokens: 100, cached_tokens: 40 }, cache, 300), 0.4));

// —— 加权聚合：不能用"比率的平均" ——
// 桶 A：1 次请求全部失败 → 成功率 0%；桶 B：999 次请求全部成功 → 100%。
// 正确口径 = 999/1000 = 99.9%，错误的"平均比率" = 50%。
const weightedPoints = [
  { requests: 1, successes: 0, prompt_tokens: 10, cached_tokens: 0, total_duration_ms: 1000, total_first_token_ms: 100 },
  { requests: 999, successes: 999, prompt_tokens: 10000, cached_tokens: 5000, total_duration_ms: 999000, total_first_token_ms: 99900 },
];
const sums = m.windowSums(weightedPoints);
check("window_sums_requests", sums.requests === 1000);
check("window_success_rate_weighted", near(m.METRIC_MAP.success.total(sums), 0.999), String(m.METRIC_MAP.success.total(sums)));
check("window_cache_rate_weighted", near(m.METRIC_MAP.cache.total(sums), 5000 / 10010), String(m.METRIC_MAP.cache.total(sums)));
check("window_avg_duration_weighted", near(m.METRIC_MAP.latency.total(sums), 1000000 / 1000), String(m.METRIC_MAP.latency.total(sums)));
check("window_zero_buckets", m.windowSums([{ requests: 0 }, { requests: 2 }]).zeroBuckets === 1);
check("window_handles_null", m.windowSums([{ requests: null, total_tokens: null }]).requests === 0);

// 分母为 0 时必须是"无数据"而不是 0%：否则空窗口会被读成"全部失败"。
check("empty_window_rate_is_null", m.METRIC_MAP.success.total(m.windowSums([])) === null);
check("zero_request_bucket_is_null", m.metricValue({ requests: 0, successes: 0 }, m.METRIC_MAP.success, 60) === null);
check("zero_request_latency_is_null", m.metricValue({ requests: 0, total_duration_ms: 0 }, m.METRIC_MAP.latency, 60) === null);

// 0 是真实读数：空闲期的 RPM 必须画成 0，不能当缺口断线。
check("zero_rpm_is_zero_not_null", m.metricValue({ requests: 0 }, rpm, 60) === 0);

// —— 残桶标记 ——
const partialSeries = m.series(
  [
    { started_at: "2026-01-01T10:00:00+08:00", requests: 10, complete: true },
    { started_at: "2026-01-01T10:01:00+08:00", requests: 3, complete: false },
  ],
  rpm,
  60,
);
check("series_marks_partial", partialSeries[1].partial === true && partialSeries[0].partial === false);
check("complete_only_drops_partial", m.completeOnly([
  { complete: true }, { complete: false }, { complete: true },
]).length === 2);
check("latest_value_finds_last_readable", m.latestValue(partialSeries)?.value === 3);
check("latest_value_skips_null", m.latestValue([{ value: 5 }, { value: null }])?.value === 5);
check("latest_value_empty_is_null", m.latestValue([]) === null);

// —— 坐标轴刻度 ——
const axis = m.axisScale([0, 3, 17, 42]);
check("axis_max_covers_data", axis.max >= 42, String(axis.max));
check("axis_ticks_include_max", near(axis.ticks[axis.ticks.length - 1], axis.max), JSON.stringify(axis.ticks));
check("axis_starts_at_zero", axis.ticks[0] === 0, JSON.stringify(axis.ticks));
// 刻度值不能出现浮点尾差。
const fractional = m.axisScale([0.1, 0.23, 0.4]);
check(
  "axis_ticks_are_clean",
  fractional.ticks.every((value) => String(value).length <= 6),
  JSON.stringify(fractional.ticks),
);
check("axis_all_zero_has_nonzero_max", m.axisScale([0, 0, 0]).max > 0, JSON.stringify(m.axisScale([0, 0, 0])));
check("axis_percent_is_fixed_0_100", m.axisScale([0.3, 0.9], { percent: true }).max === 100);
check("axis_equal_values_ok", m.axisScale([7, 7, 7]).max >= 7, JSON.stringify(m.axisScale([7, 7, 7])));

// niceStep 阶梯
check("nice_step_1", m.niceStep(0.9) === 1);
check("nice_step_2", m.niceStep(1.4) === 2);
check("nice_step_25", m.niceStep(2.2) === 2.5);
check("nice_step_10", m.niceStep(7) === 10);

// —— 时间轴标签：不重叠 + 含末尾 ——
const many = Array.from({ length: 60 }, (_, i) => ({ started_at: `2026-01-01T10:${String(i).padStart(2, "0")}:00+08:00` }));
const ticks = m.timeTicks(many, 6);
check("time_ticks_bounded", ticks.length <= 8, String(ticks.length));
check("time_ticks_include_last", ticks[ticks.length - 1].index === 59, JSON.stringify(ticks.map((t) => t.index)));
check("time_ticks_sorted", ticks.every((t, i) => i === 0 || t.index > ticks[i - 1].index));
check("time_ticks_empty_ok", m.timeTicks([]).length === 0);
// 回归：lineChart 把 series() 的产物喂给 timeTicks，那里的时间字段叫 at 而不是
// started_at。曾因此让整条 X 轴渲染成 "-"，所以两种形态都必须认。
const asSeries = m.series(
  many.map((p) => ({ ...p, requests: 1, complete: true })),
  m.METRIC_MAP.rpm,
  60,
);
const seriesTicks = m.timeTicks(asSeries, 6);
check("time_ticks_accepts_series_at", seriesTicks.every((t) => typeof t.at === "string" && t.at.includes("T")),
  JSON.stringify(seriesTicks.map((t) => t.at).slice(0, 2)));
check("time_ticks_series_at_matches_started",
  seriesTicks.length === ticks.length && seriesTicks[0].at === many[0].started_at,
  `${seriesTicks[0]?.at} vs ${many[0].started_at}`);

// —— 排行与状态码分桶 ——
check("rank_sorts_and_limits", m.rank({ a: { requests: 1 }, b: { requests: 9 }, c: { requests: 5 } }).map((r) => r.name).join(",") === "b,c,a");
check("rank_drops_zero", m.rank({ a: { requests: 0 } }).length === 0);
const groups = m.statusGroups({ "200": 5, "401": 2, "500": 1, "302": 3 });
check("status_groups_split", groups.ok === 5 && groups.client === 2 && groups.server === 1 && groups.other === 3, JSON.stringify(groups));

// —— 环比：前半段为 0 时不给百分比 ——
check("trend_null_when_no_baseline", m.trend([{ value: 0 }, { value: 0 }, { value: 5 }, { value: 5 }]) === null);
const rising = m.trend([{ value: 1 }, { value: 1 }, { value: 2 }, { value: 2 }]);
check("trend_computes_change", near(rising?.change, 1), JSON.stringify(rising));
check("trend_needs_enough_points", m.trend([{ value: 1 }, { value: 2 }]) === null);

// —— 数字格式：不能把"极小但非零"显示成 0% ——
check("percent_small_not_zero", m.formatPercentValue(0.0001) === "<0.5%", m.formatPercentValue(0.0001));
check("percent_almost_full", m.formatPercentValue(0.999) === ">99.5%", m.formatPercentValue(0.999));
check("percent_normal", m.formatPercentValue(0.4231) === "42.3%", m.formatPercentValue(0.4231));
check("percent_nan_is_dash", m.formatPercentValue(NaN) === "-");
check("compact_millions", m.formatCompactNumber(2_500_000) === "2.5M", m.formatCompactNumber(2_500_000));
check("compact_thousands", m.formatCompactNumber(12_300) === "12.3K", m.formatCompactNumber(12_300));
check("compact_small_int", m.formatCompactNumber(42) === "42");
check("duration_seconds", m.formatDurationValue(2500) === "2.50s", m.formatDurationValue(2500));
check("duration_ms", m.formatDurationValue(240) === "240ms");

const failed = Object.entries(checks).filter(([, value]) => value !== true);
console.log(JSON.stringify({ failed: failed.map(([name, detail]) => `${name} (${detail})`), total: Object.keys(checks).length }));
process.exit(failed.length ? 1 : 0);
