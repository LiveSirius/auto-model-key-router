// 活动：用量总览、按调用方/模型/Key 的分解表、服务日志尾部。

import { h, formatCount, formatCompact, formatPercent, formatRate, formatDuration, errorText } from "../dom.js";
import { api } from "../api.js";
import { card, cardHead, stat, notice, badge, loading, table, render } from "../ui.js";

function summary(metrics) {
  const total = metrics.total || {};
  const current = metrics.current_rpm ?? 0;
  const tpm = metrics.current_tpm ?? 0;
  const rows = [
    ["总请求", formatCount(total.requests)],
    ["成功率", formatPercent(total.successes, total.requests)],
    ["成功 / 失败", `${formatCount(total.successes)} / ${formatCount(total.failures)}`],
    ["重试", formatCount(total.retries)],
    ["输入 / 输出 Token", `${formatCompact(total.prompt_tokens)} / ${formatCompact(total.completion_tokens)}`],
    ["缓存 Token", formatCompact(total.cached_tokens)],
    ["当前流量", `${formatCount(current)} RPM / ${formatCompact(tpm)} TPM`],
    ["缓存率", formatRate(total.cached_token_rate)],
    ["活动请求", formatCount(metrics.active_requests ?? 0)],
    ["平均首字", total.avg_first_token_ms ? formatDuration(total.avg_first_token_ms) : "-"],
    ["平均耗时", formatDuration(total.avg_duration_ms)],
  ];
  const routerTone = { green: "good", yellow: "warn", red: "bad" }[metrics.router_status] || "muted";
  return h("div.card", {},
    cardHead("用量总览", badge("最近 1 小时", "muted"), badge(routerLabel(metrics.router_status), routerTone)),
    h("div.grid", rows.map(([label, value]) => stat(label, value))),
  );
}

function routerLabel(status) {
  if (status === "green") return "路由正常";
  if (status === "yellow") return "路由警告";
  if (status === "red") return "路由异常";
  return "路由空闲";
}

function breakdown(title, entries, emptyText, keyLabel) {
  const rows = Object.entries(entries || {})
    .map(([name, stats]) => ({ name, stats }))
    .sort((a, b) => (b.stats.requests || 0) - (a.stats.requests || 0));
  const columns = [
    { label: keyLabel, render: (row) => h("code", row.name) },
    { label: "请求", numeric: true, render: (row) => formatCount(row.stats.requests) },
    { label: "成功率", numeric: true, render: (row) => formatPercent(row.stats.successes, row.stats.requests) },
    { label: "Token", numeric: true, render: (row) => formatCompact(row.stats.total_tokens) },
    { label: "缓存率", numeric: true, render: (row) => formatRate(row.stats.cached_token_rate) },
    { label: "平均耗时", numeric: true, render: (row) => formatDuration(row.stats.avg_duration_ms) },
  ];
  return h("div.card", {}, cardHead(title, badge(`${rows.length} 项`, "muted")),
    table(columns, rows, emptyText));
}

function flattenKeys(keys) {
  const flat = {};
  for (const [model, byKey] of Object.entries(keys || {})) {
    for (const [key, stats] of Object.entries(byKey || {})) flat[`${model} / ${key}`] = stats;
  }
  return flat;
}

function logPanel() {
  const pre = h("pre.log-panel", { tabindex: "0" }, "正在读取服务日志。");
  const load = async () => {
    try {
      const data = await api.logs();
      if (data.error) { render(pre, `日志暂不可用: ${data.error}`); return; }
      const pinned = pre.scrollHeight - pre.scrollTop - pre.clientHeight <= 8;
      pre.replaceChildren(...(data.text ? data.text.split(/\r?\n/).map(lineNode) : [document.createTextNode("正在读取服务日志。")]));
      if (pinned) pre.scrollTop = pre.scrollHeight;
    } catch (error) {
      render(pre, `日志暂不可用: ${errorText(error)}`);
    }
  };
  load();
  const timer = setInterval(load, 2000);
  pre.addEventListener("remove", () => clearInterval(timer));
  // 页面重绘会丢弃旧节点，用 MutationObserver 兜住计时器释放。
  const observer = new MutationObserver(() => {
    if (!pre.isConnected) { clearInterval(timer); observer.disconnect(); }
  });
  observer.observe(document.body, { childList: true, subtree: true });
  return h("div.card", {}, cardHead("服务日志", badge("最近 64 KiB", "muted"), badge("2 秒刷新", "muted")), pre);
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

export function renderActivity(xtx) {
  const { store } = xtx;
  if (store.health?.local_auth_enabled && !store.authorized) {
    return h("div.stack", {}, h("div.page-head", h("h1", "活动")), notice("需要本地鉴权 Key 才能读取统计。", "warn"));
  }
  if (!store.metrics || !store.metrics.total) {
    return h("div.stack", {},
      h("div.page-head", {}, h("div", {}, h("h1", "活动"), h("p.sub", "按 CLI 统计口径汇总最近一小时的请求用量。"))),
      store.metricsError ? notice(`指标读取失败: ${store.metricsError}`, "error") : loading("正在读取指标…"),
      logPanel(),
    );
  }
  const metrics = store.metrics;
  return h("div.stack", {},
    h("div.page-head", {},
      h("div", {}, h("h1", "活动"), h("p.sub", "按 CLI 统计口径汇总最近一小时的请求用量。")),
      h("div.spacer"),
      badge("实时更新", "good"),
    ),
    summary(metrics),
    h("div.grid.wide", {},
      breakdown("调用方", metrics.caller_types, "暂无调用方数据。", "调用方"),
      breakdown("模型", metrics.models, "暂无模型调用数据。", "模型"),
      breakdown("Key", flattenKeys(metrics.keys), "暂无 Key 调用数据。", "模型 / Key"),
    ),
    logPanel(),
  );
}
