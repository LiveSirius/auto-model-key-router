// 成本估算口径回归探针：直接驱动 webui/pricing.js 的纯函数。
// 用法：node webui/probes/webui_pricing_probe.mjs，结果以 JSON 打到 stdout，失败时退出码 1。
//
// 为什么值得单独锁：成本算错的后果不是"图不好看"，而是**给出一个看起来精确的错数字**。
// 这里钉住三件最容易出错的事：
//   1. 缓存 token 不能既按输入价又按缓存价计费（重复计费）；
//   2. 拿不到单价时必须回 null 而不是 0（0 会被读成"免费"）；
//   3. 匹配要大小写不敏感、要能穿透日期后缀与供应商前缀。

import { pathToFileURL } from "node:url";
import path from "node:path";

const MODULE = path.resolve(import.meta.dirname, "../pricing.js");
const m = await import(pathToFileURL(MODULE).href);

const checks = {};
const near = (a, b, epsilon = 1e-12) =>
  Number.isFinite(a) && Number.isFinite(b) && Math.abs(a - b) < epsilon;
const check = (name, ok, detail = "") => {
  checks[name] = ok ? true : `FAILED${detail ? `: ${detail}` : ""}`;
};

// —— 目录归一化 ——
const document = {
  version: 1,
  source: "https://models.dev/api.json",
  updated_at: "2026-01-02T03:04:05Z",
  error: null,
  models: {
    "gpt-4o": { input: 2.5, output: 10, cache_read: 1.25 },
    "claude-sonnet-4-5": { input: 3, output: 15, cache_read: 0.3, cache_write: 3.75 },
    "no-cache-price": { input: 1, output: 2 },
    "bad-entry": { input: "x", output: 2 },
    "null-input": { input: null, output: 3 },
  },
};
const index = m.normalizeCatalog(document);
check("normalize_returns_index", index !== null);
check("normalize_keeps_valid_models", index?.models.size === 3, String(index?.models.size));
check("normalize_drops_non_numeric_input", !index?.models.has("bad-entry"));
check("normalize_drops_null_input", !index?.models.has("null-input"));
check("normalize_reports_updated_at", index?.updatedAt === "2026-01-02T03:04:05Z");
check("normalize_missing_cache_price_is_null", index?.models.get("no-cache-price")?.cacheRead === null);

// 形状不符时必须返回 null（界面据此显示 "—"，而不是把成本当 0）。
check("normalize_rejects_null", m.normalizeCatalog(null) === null);
check("normalize_rejects_missing_models", m.normalizeCatalog({ version: 1 }) === null);
check("normalize_rejects_empty_models", m.normalizeCatalog({ models: {} }) === null);
check("normalize_rejects_non_object_models", m.normalizeCatalog({ models: "nope" }) === null);
check("normalize_rejects_all_invalid", m.normalizeCatalog({ models: { a: { input: "x", output: "y" } } }) === null);

// —— 匹配：大小写、供应商前缀、日期后缀 ——
check("lookup_exact", m.lookupPrice(index, "gpt-4o")?.input === 2.5);
check("lookup_is_case_insensitive", m.lookupPrice(index, "GPT-4O")?.input === 2.5);
check("lookup_trims", m.lookupPrice(index, "  gpt-4o  ")?.input === 2.5);
check("lookup_strips_vendor_prefix", m.lookupPrice(index, "openai/gpt-4o")?.input === 2.5);
check("lookup_unknown_is_null", m.lookupPrice(index, "not-a-model") === null);
check("lookup_empty_is_null", m.lookupPrice(index, "") === null);
check("lookup_null_id_is_null", m.lookupPrice(index, null) === null);
check("lookup_without_index_is_null", m.lookupPrice(null, "gpt-4o") === null);

// 日期后缀：本地常见写法是带日期，目录里往往只有不带日期的那条。
const dated = m.normalizeCatalog({
  models: { "gpt-4o-mini": { input: 0.15, output: 0.6 } },
});
check("lookup_strips_iso_date_suffix", m.lookupPrice(dated, "gpt-4o-mini-2024-07-18")?.input === 0.15);
check("lookup_strips_compact_date_suffix", m.lookupPrice(dated, "gpt-4o-mini-20240718")?.input === 0.15);
// 反向：目录里只有带日期的，本地写不带的也应命中（候选链里有原名即可，这里验证不误伤）。
const datedOnly = m.normalizeCatalog({
  models: { "claude-3-7-sonnet-20250219": { input: 3, output: 15 } },
});
check("lookup_exact_dated_still_works", m.lookupPrice(datedOnly, "claude-3-7-sonnet-20250219")?.input === 3);
// 日期后缀只应剥掉一层，且不能把正常名字剥坏。
check("lookup_does_not_strip_non_date", m.lookupPrice(dated, "gpt-4o-mini-latest") === null);

// —— 核心：成本公式 ——
// 单价 USD / 1M token。
// prompt 1,000,000（其中缓存读 400,000、缓存写 100,000 → fresh 500,000）
// completion 200,000
// 期望 = (500000*3 + 400000*0.3 + 100000*3.75 + 200000*15) / 1e6
//      = (1500000 + 120000 + 375000 + 3000000) / 1e6 = 4.995
const sonnet = index.models.get("claude-sonnet-4-5");
const usage = {
  prompt_tokens: 1_000_000,
  completion_tokens: 200_000,
  cached_tokens: 400_000,
  cache_read_input_tokens: 400_000,
  cache_creation_input_tokens: 100_000,
};
const cost = m.estimateCost(sonnet, usage);
check("cost_full_breakdown", near(cost, 4.995), String(cost));

// **重复计费陷阱**：uncached_prompt_tokens 在 Anthropic 语义下等于
// input + cache_creation，若拿它乘输入价，那 100,000 的缓存写会被算两次。
// 正确口径下成本必须严格小于"prompt 全按输入价 + completion 按输出价"。
const naive = (usage.prompt_tokens * sonnet.input + usage.completion_tokens * sonnet.output) / 1e6;
check("cost_does_not_double_bill_cache_write", cost < naive, `${cost} !< ${naive}`);
// 若把缓存写误按输入价算，会得到 (500000+100000)*3/1e6 + 120000/1e6 + 3000000/1e6 = 4.92…… 
// 真正的判据是缓存写必须用 cache_write 价（3.75）而不是输入价（3）。
check("cost_uses_cache_write_price", near(cost, (500000 * 3 + 400000 * 0.3 + 100000 * 3.75 + 200000 * 15) / 1e6));

// OpenAI 语义：只有 cached_tokens（cache_read_input_tokens 为 0），且没有缓存写。
const gpt4o = index.models.get("gpt-4o");
const openaiUsage = {
  prompt_tokens: 1_000_000,
  completion_tokens: 1_000_000,
  cached_tokens: 500_000,
  cache_read_input_tokens: 0,
  cache_creation_input_tokens: 0,
};
// (500000*2.5 + 500000*1.25 + 1000000*10) / 1e6 = 1.25 + 0.625 + 10 = 11.875
check("cost_openai_falls_back_to_cached_tokens", near(m.estimateCost(gpt4o, openaiUsage), 11.875),
  String(m.estimateCost(gpt4o, openaiUsage)));

// 缓存价缺失 → 回退输入价（不能当成免费）。
// (500000*1 + 500000*1) / 1e6 = 1.0（prompt 全 1M 按输入价 1 算，因为缓存读回退到输入价）
const noCache = index.models.get("no-cache-price");
const noCacheUsage = { prompt_tokens: 1_000_000, completion_tokens: 0, cached_tokens: 500_000 };
check("cost_missing_cache_price_falls_back_to_input", near(m.estimateCost(noCache, noCacheUsage), 1.0),
  String(m.estimateCost(noCache, noCacheUsage)));

// 零用量 = $0（真实存在：失败的请求没有 token）。
check("cost_zero_usage_is_zero", m.estimateCost(gpt4o, {}) === 0);

// 负数/NaN/字符串一律当 0，绝不产生负成本。
check("cost_negative_tokens_clamped", m.estimateCost(gpt4o, {
  prompt_tokens: -100, completion_tokens: -5,
}) === 0);
check("cost_nan_tokens_ignored", m.estimateCost(gpt4o, { prompt_tokens: "abc", completion_tokens: NaN }) === 0);
// prompt 小于缓存量（上游字段自相矛盾）时 fresh 兜到 0，不出现负成本。
const contradictory = m.estimateCost(gpt4o, {
  prompt_tokens: 10, cached_tokens: 1000, cache_read_input_tokens: 1000,
});
check("cost_handles_inconsistent_cache_tokens", contradictory >= 0, String(contradictory));

// 没有条目/没有用量 → null（"—"），不是 0。
check("cost_without_entry_is_null", m.estimateCost(null, usage) === null);
check("cost_without_usage_is_null", m.estimateCost(gpt4o, null) === null);

// —— 单请求成本：按 upstream_model_id 匹配 ——
const item = {
  model_id: "本地路由名",
  upstream_model_id: "gpt-4o",
  prompt_tokens: 1_000_000,
  completion_tokens: 1_000_000,
  cached_tokens: 500_000,
};
check("request_cost_matches_upstream_model", near(m.requestCost(index, item), 11.875),
  String(m.requestCost(index, item)));
// 只有本地 model_id、没有 upstream_model_id 时拿不到价格（本地名不可信）。
check("request_cost_requires_upstream_model", m.requestCost(index, { model_id: "gpt-4o" }) === null);
check("request_cost_unknown_upstream_is_null", m.requestCost(index, { ...item, upstream_model_id: "nope" }) === null);
check("request_cost_null_item_is_null", m.requestCost(index, null) === null);

// —— 聚合与合计 ——
const entries = {
  "gpt-4o": { prompt_tokens: 1_000_000, completion_tokens: 0, cached_tokens: 0 },
  "not-priced": { prompt_tokens: 5_000_000, completion_tokens: 0, cached_tokens: 0 },
  "claude-sonnet-4-5": { prompt_tokens: 1_000_000, completion_tokens: 0, cached_tokens: 0, cache_read_input_tokens: 0 },
};
const summed = m.sumCost(index, entries);
// gpt-4o: 1e6*2.5/1e6 = 2.5；claude: 1e6*3/1e6 = 3；合计 5.5，2 项有价 / 共 3 项。
check("sum_cost_total", near(summed.total, 5.5), String(summed.total));
check("sum_cost_priced_count", summed.priced === 2, String(summed.priced));
check("sum_cost_total_count", summed.total_count === 3, String(summed.total_count));
check("sum_cost_empty_is_zero", m.sumCost(index, {}).total === 0);
check("sum_cost_null_entries", m.sumCost(index, null).total_count === 0);

// —— 渲染：金额与单价 ——
check("format_cost_null_is_dash", m.formatCost(null) === "—");
check("format_cost_undefined_is_dash", m.formatCost(undefined) === "—");
check("format_cost_nan_is_dash", m.formatCost(NaN) === "—");
check("format_cost_zero", m.formatCost(0) === "$0");
check("format_cost_tiny", m.formatCost(0.00001) === "<$0.0001", m.formatCost(0.00001));
check("format_cost_sub_cent", m.formatCost(0.001234) === "$0.00123", m.formatCost(0.001234));
check("format_cost_small", m.formatCost(0.5) === "$0.5000", m.formatCost(0.5));
check("format_cost_normal", m.formatCost(12.3456) === "$12.35", m.formatCost(12.3456));
check("format_cost_thousands", m.formatCost(12345.678) === "$12,345.68", m.formatCost(12345.678));
// 绝不用科学计数法：运维读不懂 "1.2e-4"。
check("format_cost_never_scientific", !/e/i.test(m.formatCost(0.00001)) && !/e/i.test(m.formatCost(0.0000012)));

check("format_cost_exact_null_is_dash", m.formatCostExact(null) === "—");
check("format_cost_exact_precision", m.formatCostExact(0.0001234).startsWith("$0.000123"),
  m.formatCostExact(0.0001234));
check("format_price_null_is_dash", m.formatPrice(null) === "—");
check("format_price_integer", m.formatPrice(3) === "$3", m.formatPrice(3));
check("format_price_fraction", m.formatPrice(0.15) === "$0.15", m.formatPrice(0.15));
check("format_price_zero_is_zero", m.formatPrice(0) === "$0", m.formatPrice(0));

// —— 加载状态：失败不能抛异常，也不能把成本变成 0 ——
const failing = async () => { throw new Error("HTTP 503"); };
const missing = await m.loadPricing(failing);
check("load_failure_returns_without_throwing", missing === null || typeof missing === "object");
check("load_failure_reports_error", typeof m.pricingStatus().error === "string",
  JSON.stringify(m.pricingStatus().error));

const loaded = await m.loadPricing(async () => document);
check("load_success_index", loaded?.models.size === 3, String(loaded?.models.size));
// 已有索引后不再重复请求（并发/轮询不会反复拉 230 KB）。
let calls = 0;
await m.loadPricing(async () => { calls += 1; return document; });
check("load_is_cached_after_success", calls === 0, String(calls));
check("current_index_matches", m.currentIndex() === loaded);

const failed = Object.values(checks).filter((value) => value !== true);
console.log(JSON.stringify({ module: "webui/pricing.js", checks, failures: failed.length }, null, 2));
if (failed.length) {
  console.error(`\n${failed.length} 项成本口径断言失败`);
  process.exit(1);
}
