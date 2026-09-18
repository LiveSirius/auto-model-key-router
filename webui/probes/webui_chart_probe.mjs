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
  "../chart-math.js",
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
for (const hours of [1, 6, 24, 72, 168, 720, 2160, 4320, 8760]) {
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

// —— 时间范围表：实时窗口与历史窗口分开 ——
// 实时档（1h-7d）给概览用；历史档（1 个月起）给用量统计用。
// 「全部」不在表里（它的小时数是运行时从最早记录推导的），但必须能从 rangeSpec 解析。
check("time_ranges_are_realtime", m.TIME_RANGES.map((r) => r.hours).join(",") === "1,6,24,72,168");
check("long_ranges_1m_3m_6m_1y", m.LONG_RANGES.map((r) => r.hours).join(",") === "720,2160,4320,8760");
check("usage_ranges_include_all", m.USAGE_RANGES.at(-1).hours === m.ALL_HISTORY);
check("usage_ranges_count", m.USAGE_RANGES.length === m.TIME_RANGES.length + m.LONG_RANGES.length + 1);
check("max_history_covers_1y", m.MAX_HISTORY_HOURS === 8760);
// 每个历史档都必须能选出满足点数上限的桶，否则切到那一档就会拿到 422。
for (const range of m.LONG_RANGES) {
  const spec = m.rangeSpec(range.hours, {});
  const bucket = m.pickBucketSeconds(spec.hours);
  check(`long_range_${range.short}_within_cap`,
    Math.ceil((spec.hours * 3600) / bucket) + 1 <= m.MAX_SERIES_POINTS,
    `${range.short} 取 ${bucket}s 桶会超上限`);
}
// 「全部」的跨度推导：正常取整、超过上限夹紧、没有历史返回 null。
const now = Date.parse("2026-06-01T00:00:00+08:00");
check("history_hours_rounds_up", m.historyHours("2026-05-31T22:30:00+08:00", now) === 2,
  String(m.historyHours("2026-05-31T22:30:00+08:00", now)));
check("history_hours_clamps_to_cap", m.historyHours("2000-01-01T00:00:00+08:00", now) === m.MAX_HISTORY_HOURS,
  String(m.historyHours("2000-01-01T00:00:00+08:00", now)));
check("history_hours_null_when_empty", m.historyHours(null, now) === null);
check("history_hours_null_when_future", m.historyHours("2026-07-01T00:00:00+08:00", now) === null);
// 「全部」的徽标必须能说明"只覆盖到上限"，否则会被读成真的是全部。
const allSpec = m.rangeSpec(m.ALL_HISTORY, { allHours: m.MAX_HISTORY_HOURS });
check("all_range_is_marked_truncated", allSpec.all === true && allSpec.truncated === true);
check("all_range_within_cap_not_truncated",
  m.rangeSpec(m.ALL_HISTORY, { allHours: 100 }).truncated === false);
check("all_range_falls_back_to_cap", m.rangeSpec(m.ALL_HISTORY, {}).hours === m.MAX_HISTORY_HOURS);

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

// —— 缺口桥接 ——
// 回归：15 秒一桶的稀疏流量下，均值型指标（耗时/首字）在无请求的桶上是 null，
// 只按"连续非空"切段会让一条 1 小时曲线碎成几十段。缺口必须被桥接起来，
// 但首尾缺口不能凭空连出去（外面没有可连的读数）。
const bridged = m.gapBridges([
  { value: 1 }, { value: null }, { value: null }, { value: 2 }, { value: 3 }, { value: null },
]);
check("gap_bridges_single_gap", bridged.length === 1, JSON.stringify(bridged));
// 用可选链：实现退化时断言应当报"这行不对"，而不是让整个探针抛异常、丢掉其余用例。
check("gap_bridge_endpoints", bridged[0]?.from === 0 && bridged[0]?.to === 3, JSON.stringify(bridged[0]));
// 尾部的 null 之后没有可读点 ⇒ 不产生桥；否则会从最后一个实测点画到图外。
check("gap_no_bridge_without_right_anchor", bridged.every((b) => b.to !== 5));
// 开头就是缺口时同理：左边没有锚点。
check("gap_no_bridge_without_left_anchor",
  m.gapBridges([{ value: null }, { value: 1 }, { value: 2 }]).length === 0);
// 相邻两个可读点之间没有缺口 ⇒ 不多画一条与实线重叠的虚线。
check("gap_ignores_adjacent_points", m.gapBridges([{ value: 1 }, { value: 2 }]).length === 0);
// 0 是真实读数（空闲），不是缺口：不能被桥接，更不该被当成缺失。
check("gap_zero_is_readable_not_gap",
  m.gapBridges([{ value: 0 }, { value: 0 }, { value: 0 }]).length === 0);
check("gap_null_and_zero_differ", m.readable({ value: 0 }) === true && m.readable({ value: null }) === false);
check("gap_nan_is_not_readable", m.readable({ value: NaN }) === false);
check("gap_empty_is_empty", m.gapBridges([]).length === 0 && m.gapBridges(null).length === 0);
// 真实形状：稀疏耗时序列（每三个桶一个读数）必须被桥接成一条连续折线。
const sparse = Array.from({ length: 30 }, (_, i) => ({ value: i % 3 === 0 ? 100 + i : null }));
check("gap_sparse_series_bridges_every_gap", m.gapBridges(sparse).length === 9, String(m.gapBridges(sparse).length));

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

// —— 热力图：星期 × 半小时的落格 ——
// 最容易错的是"星期几/几点"取自哪里：后端时间戳带 +08:00，若读 Date 的
// getDay/getHours，浏览器时区一变整张矩阵就平移。这里锁死按 Asia/Shanghai 取。
// 2026-01-01 是周四，01-04 是周日。一格 30 分钟 ⇒ slot = 小时*2 + 半小时位。
check("slot_beijing_thursday_10", JSON.stringify(m.beijingSlot("2026-01-01T10:00:00+08:00")) === JSON.stringify({ weekday: 3, slot: 20 }),
  JSON.stringify(m.beijingSlot("2026-01-01T10:00:00+08:00")));
check("slot_sunday_23", JSON.stringify(m.beijingSlot("2026-01-04T23:30:00+08:00")) === JSON.stringify({ weekday: 6, slot: 47 }),
  JSON.stringify(m.beijingSlot("2026-01-04T23:30:00+08:00")));
// 同一时刻换成 UTC 写法，格子必须完全一致。
check("slot_same_for_utc_form",
  JSON.stringify(m.beijingSlot("2026-01-01T10:00:00+08:00")) === JSON.stringify(m.beijingSlot("2026-01-01T02:00:00Z")));
// 00:30 属于第 1 格（00:30–01:00），不能被四舍五入回 0 点那一格。
check("slot_hour_zero_not_rounded", m.beijingSlot("2026-01-01T00:30:00+08:00").slot === 1);
// 半小时粒度是这次改动的全部意义：10:00 与 10:30 必须落在不同格子，
// 否则等于没细化，读数还是整点口径。
check("slot_half_hour_is_distinct",
  m.beijingSlot("2026-01-01T10:30:00+08:00").slot === 21 &&
  m.beijingSlot("2026-01-01T10:59:00+08:00").slot === 21);
check("slot_bad_input_is_null", m.beijingSlot("not-a-date") === null);
check("slot_label_formats_half_hour", m.slotLabel(21) === "10:30" && m.slotLabel(0) === "00:00",
  `${m.slotLabel(21)} / ${m.slotLabel(0)}`);
check("slot_count_is_48", m.HEATMAP_SLOTS === 48, String(m.HEATMAP_SLOTS));

const heatPoints = [
  { started_at: "2026-01-01T10:00:00+08:00", requests: 5, total_tokens: 500, complete: true },
  { started_at: "2026-01-01T10:00:00+08:00", requests: 7, total_tokens: 700, complete: true },
  { started_at: "2026-01-01T10:00:00+08:00", requests: 3, total_tokens: 300, complete: false },
  { started_at: "2026-01-04T23:00:00+08:00", requests: 100, total_tokens: 9000, complete: true },
];
const cells = m.heatmapCells(heatPoints, { value: (p) => p.requests });
check("heat_cells_shape", cells.length === 7 && cells.every((row) => row.length === 48));
check("heat_sums_same_slot", cells[3][20].value === 15, String(cells[3][20].value));
check("heat_counts_buckets", cells[3][20].buckets === 3, String(cells[3][20].buckets));
check("heat_marks_partial", cells[3][20].partial === true);
check("heat_other_cell_untouched", cells[6][46].value === 100 && cells[6][46].buckets === 1);
// 没数据 ≠ 值为 0：buckets 为 0 才是"窗口未覆盖"，这一条决定了格子画斜纹还是纯色。
check("heat_coverless_has_zero_buckets", cells[0][0].buckets === 0 && cells[0][0].value === 0);
check("heat_cell_has_data_is_not_coverless", cells[3][20].buckets > 0);
check("heat_undefined_point_ignored", m.heatmapCells([undefined, {}])[0][0].buckets === 0);
check("heat_tokens_metric", m.heatmapCells(heatPoints, { value: m.HEATMAP_METRIC_MAP.tokens.pick })[3][20].value === 1500);
// 相邻半小时不能互相污染：这是 30 分钟粒度下最容易犯的错（把两个桶并进一格）。
const neighbour = m.heatmapCells([
  { started_at: "2026-01-01T10:00:00+08:00", requests: 5, complete: true },
  { started_at: "2026-01-01T10:30:00+08:00", requests: 9, complete: true },
], { value: (p) => p.requests });
check("heat_half_hour_not_merged", neighbour[3][20].value === 5 && neighbour[3][21].value === 9,
  `${neighbour[3][20].value} / ${neighbour[3][21].value}`);

// 色阶上限只看有数据的格子：空窗口不能因为"未覆盖"就给出非零上限。
check("heat_scale_uses_data_max", m.heatmapScale(cells) === 100, String(m.heatmapScale(cells)));
check("heat_scale_zero_when_no_data", m.heatmapScale(m.heatmapCells([])) === 0);
check("heat_scale_ignores_coverless", m.heatmapScale(m.heatmapCells([
  { started_at: "2026-01-01T10:00:00+08:00", requests: 4, complete: true },
])) === 4);

// 档位：0 单独一档（真·空闲），最大值必须落在最高档。
check("heat_level_zero_is_zero", m.heatmapLevel(0, 100) === 0);
check("heat_level_max_is_top", m.heatmapLevel(100, 100) === m.HEAT_LEVELS);
check("heat_level_monotonic", [1, 25, 50, 75, 100].every((v, i, arr) =>
  i === 0 || m.heatmapLevel(v, 100) >= m.heatmapLevel(arr[i - 1], 100)));
check("heat_level_no_data_is_zero", m.heatmapLevel(5, 0) === 0);
check("heat_levels_has_zero_and_top", m.HEAT_LEVELS >= 2);

// —— 历史聚合：按天 / 按时段 / 累计 / 前后对比 ——
// 这些是「用量统计」页的读数来源，算错不会崩，只会让历史结论错。
const histPoints = [
  // 同一天两个桶：必须并成一天，且加权量直接相加。
  { started_at: "2026-01-01T00:00:00+08:00", ended_at: "2026-01-01T00:30:00+08:00", requests: 10, successes: 9, failures: 1, retries: 2, prompt_tokens: 60, completion_tokens: 40, total_tokens: 100, cached_tokens: 20, total_duration_ms: 1000, total_first_token_ms: 100, complete: true },
  { started_at: "2026-01-01T00:30:00+08:00", ended_at: "2026-01-01T01:00:00+08:00", requests: 30, successes: 30, failures: 0, retries: 0, prompt_tokens: 180, completion_tokens: 120, total_tokens: 300, cached_tokens: 60, total_duration_ms: 6000, total_first_token_ms: 300, complete: true },
  { started_at: "2026-01-02T14:00:00+08:00", ended_at: "2026-01-02T14:30:00+08:00", requests: 5, successes: 5, failures: 0, retries: 0, prompt_tokens: 30, completion_tokens: 20, total_tokens: 50, cached_tokens: 10, total_duration_ms: 500, total_first_token_ms: 50, complete: false },
];
const dayList = m.dailyUsage(histPoints);
check("daily_merges_same_day", dayList.length === 2, `得到 ${dayList.length} 天`);
check("daily_sums_requests", dayList[0].requests === 40, String(dayList[0].requests));
check("daily_sums_tokens", dayList[0].total_tokens === 400, String(dayList[0].total_tokens));
// 日期要用北京时间切分：UTC 的 2026-01-01T16:30Z 属于北京的 1 月 2 日。
check("daily_uses_beijing_day",
  m.dailyUsage([{ started_at: "2026-01-01T16:30:00Z", requests: 1, complete: true }])[0].date === "2026-01-02",
  m.dailyUsage([{ started_at: "2026-01-01T16:30:00Z", requests: 1, complete: true }])[0].date);
check("daily_sorted_ascending", dayList[0].date < dayList[1].date);
// 累加中的尾桶必须被标记，否则最后一天会被读成"用量骤降"。
check("daily_marks_partial_day", dayList[1].partial === true && dayList[0].partial === false);
check("daily_empty_is_empty", m.dailyUsage([]).length === 0);
check("daily_ignores_undated", m.dailyUsage([undefined, {}]).length === 0);

// 日内时段：24 格，北京时间。
const profile = m.hourlyProfile(histPoints);
check("hourly_profile_is_24", profile.length === 24);
check("hourly_buckets_by_beijing_hour", profile[0].value === 40 && profile[14].value === 5,
  `${profile[0].value} / ${profile[14].value}`);
check("hourly_empty_hours_are_zero", profile[8].value === 0 && profile[8].buckets === 0);
// 自定义取值（Token 口径）必须被采纳。
check("hourly_custom_value",
  m.hourlyProfile(histPoints, { value: (p) => p.total_tokens })[0].value === 400,
  String(m.hourlyProfile(histPoints, { value: (p) => p.total_tokens })[0].value));

// 累计曲线：缺失桶不参与累加，也不把总量重置为 0。
const cumulative = m.cumulativeSeries(m.series([
  { started_at: "2026-01-01T00:00:00+08:00", requests: 10, complete: true },
  { started_at: "2026-01-01T00:15:00+08:00", requests: null, complete: true },
  { started_at: "2026-01-01T00:30:00+08:00", requests: 30, complete: true },
], m.METRIC_MAP.requests, 900));
check("cumulative_running_total", cumulative[0].value === 10 && cumulative[2].value === 40,
  `${cumulative[0].value} / ${cumulative[2].value}`);
// 中间点缺失时累计值不变（沿用上一个总量），而不是掉回 0。
check("cumulative_skips_missing", cumulative[1].value === 10, String(cumulative[1].value));
check("requests_metric_is_unscaled",
  m.metricValue({ requests: 30 }, m.METRIC_MAP.requests, 15) === 30,
  String(m.metricValue({ requests: 30 }, m.METRIC_MAP.requests, 15)));

// 前后对半对比：只统计已完结桶（残桶会把"后半段"拖低，让趋势看起来在跌）。
const compare = m.splitCompare(m.series([
  { started_at: "2026-01-01T00:00:00+08:00", requests: 10, complete: true },
  { started_at: "2026-01-01T00:15:00+08:00", requests: 20, complete: true },
  { started_at: "2026-01-01T00:30:00+08:00", requests: 60, complete: true },
  { started_at: "2026-01-01T00:45:00+08:00", requests: 60, complete: true },
], m.METRIC_MAP.requests, 900));
check("split_compare_halves", compare.before === 30 && compare.after === 120,
  `${compare.before} / ${compare.after}`);
check("split_compare_change", near(compare.change, 3), String(compare.change));
check("split_compare_too_short_is_null",
  m.splitCompare(m.series([{ started_at: "2026-01-01T00:00:00+08:00", requests: 1, complete: true }],
    m.METRIC_MAP.requests, 900)) === null);

const failed = Object.entries(checks).filter(([, value]) => value !== true);
console.log(JSON.stringify({ failed: failed.map(([name, detail]) => `${name} (${detail})`), total: Object.keys(checks).length }));
process.exit(failed.length ? 1 : 0);
