// 概览：统一模型卡片、用量总览、趋势图。

import { h, formatCount, formatCompact, formatPercent, formatRate, formatDuration, errorText, copyText } from "../dom.js";
import { api } from "../api.js";
import { card, cardHead, stat, notice, badge, empty, loading, render, buttonNode, toast } from "../ui.js";

const METRICS = [
  { id: "rpm", label: "RPM", unit: "次/分", value: (p) => p.current_rpm, pick: (p) => p.requests, format: (v) => formatCount(v) },
  { id: "tpm", label: "TPM", unit: "Token/分", value: (p) => p.current_tpm, pick: (p) => p.total_tokens, format: (v) => formatCompact(v) },
  { id: "cache", label: "缓存率", unit: "", value: (p) => (p.cached_token_rate ?? 0) * 100, pick: (p) => (p.cached_token_rate ?? 0) * 100, format: (v) => `${v.toFixed(1)}%` },
];

// 每分钟一个桶时，桶内请求数就是 RPM；Token 数就是 TPM。
function pointValue(point, metric) {
  if (metric.pick) return metric.pick(point);
  return metric.value(point);
}

function niceMaximum(values) {
  const max = Math.max(...values, 1);
  const magnitude = 10 ** Math.floor(Math.log10(max));
  const normalized = max / magnitude;
  const interval = normalized <= 1 ? 1 : normalized <= 2 ? 2 : normalized <= 5 ? 5 : 10;
  return interval * magnitude;
}

function chart(points, metric) {
  const width = 640;
  const height = 160;
  const pad = 16;
  const values = points.map((point) => pointValue(point, metric));
  const max = metric.id === "cache" ? 100 : niceMaximum(values);
  const step = points.length > 1 ? (width - pad * 2) / (points.length - 1) : 0;
  const x = (index) => (points.length === 1 ? width / 2 : pad + index * step);
  const y = (value) => height - pad - (Math.max(0, Math.min(value, max)) / max) * (height - pad * 2);

  const segments = [];
  let current = [];
  points.forEach((point, index) => {
    const value = values[index];
    if (value === null || value === undefined) {
      if (current.length) segments.push(current);
      current = [];
      return;
    }
    current.push(`${x(index).toFixed(1)},${y(value).toFixed(1)}`);
  });
  if (current.length) segments.push(current);

  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("viewBox", `0 0 ${width} ${height}`);
  svg.setAttribute("class", "chart");
  svg.setAttribute("role", "img");
  svg.setAttribute("aria-label", `${metric.label} 趋势`);

  const ns = (tag, attrs) => {
    const node = document.createElementNS("http://www.w3.org/2000/svg", tag);
    for (const [key, value] of Object.entries(attrs)) node.setAttribute(key, value);
    return node;
  };
  for (const ratio of [0, 0.5, 1]) {
    svg.append(ns("line", { class: "axis", x1: pad, x2: width - pad, y1: y(max * ratio), y2: y(max * ratio) }));
    const text = ns("text", { class: "scale-label", x: pad, y: y(max * ratio) - 4 });
    text.textContent = metric.id === "cache" ? `${Math.round(max * ratio)}%` : formatCompact(Math.round(max * ratio));
    svg.append(text);
  }
  for (const segment of segments) {
    if (segment.length === 1) {
      const [cx, cy] = segment[0].split(",");
      svg.append(ns("circle", { class: "dot", cx, cy, r: 2.5 }));
    } else {
      svg.append(ns("polyline", { class: "line", points: segment.join(" ") }));
    }
  }
  return svg;
}

function unifiedCard(xtx) {
  const { store, navigate } = xtx;
  const unified = store.health?.unified_model;
  const models = store.health?.models || [];
  const target = unified?.default?.primary;
  const body = unified
    ? h("div.stack.tight", {},
        h("div.inline", {},
          badge("已启用", "good"),
          h("span.muted", target
            ? `${target.key ? `固定 Key · ${target.key}` : "自动路由"} · ${1 + (unified.default?.fallback ? 1 : 0)} 个目标`
            : "尚未配置统一路由"),
        ),
        h("div.mono", target?.model || "-"),
      )
    : h("div.stack.tight", {}, badge("未启用", "muted"), h("span.muted", "请求统一入口时按下方模型路由分发。"));
  return h("div.card.is-lift", { role: "link", tabindex: "0", onClick: () => navigate("unified") },
    cardHead("统一模型", h("span.badge.muted", "统一入口")),
    body,
  );
}

function metricsCard(xtx) {
  const { store, navigate } = xtx;
  const total = store.metrics?.total;
  const rows = total
    ? [
        ["请求", formatCount(total.requests)],
        ["Token", formatCompact(total.total_tokens)],
        ["缓存命中", formatRate(total.cached_token_rate)],
        ["RPM / TPM", `${formatCount(store.metrics.current_rpm)} / ${formatCompact(store.metrics.current_tpm)}`],
        ["平均延迟", formatDuration(total.avg_duration_ms)],
        ["成功率", formatPercent(total.successes, total.requests)],
      ]
    : null;
  return h("div.card.is-lift", { role: "link", tabindex: "0", onClick: () => navigate("activity") },
    cardHead("数据总览",
      store.metricsError && total ? badge("上次成功数据", "warn") : badge("最近 60 分钟", "muted"),
    ),
    rows
      ? h("div.grid", rows.map(([label, value]) => stat(label, value)))
      : h("div", store.metricsError ? notice(`指标暂不可用: ${store.metricsError}`, "warn") : loading("正在读取指标…")),
  );
}

// 同步返回卡片节点、异步填充内容：h() 只接受节点，不能返回 Promise。
function trendCard(xtx) {
  const host = h("div.card", {}, cardHead("近一小时用量", badge("每分钟采样", "muted")),
    h("div", loading("正在读取趋势…")));
  let metricId = "rpm";
  const load = async () => {
    try {
      const data = await api.series(1, 60);
      const points = data.points || [];
      if (!points.length) { render(host, cardHead("近一小时用量"), empty("暂无趋势数据。")); return; }
      const metric = METRICS.find((item) => item.id === metricId);
      const values = points.map((point) => pointValue(point, metric));
      const latest = [...values].reverse().find((value) => value !== null && value !== undefined);
      render(host,
        cardHead("近一小时用量",
          h("div.inline", {}, METRICS.map((item) => buttonNode(item.label, {
            small: true,
            variant: item.id === metricId ? "" : "text",
            "aria-pressed": String(item.id === metricId),
            onClick: () => { metricId = item.id; load(); },
          }))),
        ),
        h("div.chart-wrap", {},
          h("div.row-between", {},
            h("span.muted", "最新"),
            h("strong", latest === undefined ? "-" : `${metric.format(latest)}${metric.unit ? ` ${metric.unit}` : ""}`),
          ),
          chart(points, metric),
        ),
        h("p.muted", "按分钟聚合的最近一小时用量；缓存率为每分钟的缓存命中占比。"),
      );
    } catch (error) {
      render(host, cardHead("近一小时用量"), notice(`趋势读取失败: ${errorText(error)}`, "error"));
    }
  };
  load();
  return host;
}

export function renderOverview(xtx) {
  const { store, askLogin } = xtx;
  if (store.connectionError) {
    return h("div.stack", {},
      h("div.page-head", h("div", {}, h("h1", "概览"), h("p.sub", "本机 AMKR 服务状态与实时用量。"))),
      notice(`无法连接 AMKR 服务: ${store.connectionError}`, "error"),
      buttonNode("重新连接", { onClick: () => xtx.refreshHealth() }),
    );
  }
  if (store.health?.local_auth_enabled && !store.authorized) {
    return h("div.stack", {}, h("div.page-head", h("h1", "概览")), notice("需要本地鉴权 Key 才能读取用量。", "warn"));
  }
  return h("div.stack", {},
    h("div.page-head", {},
      h("div", {}, h("h1", "概览"), h("p.sub", "本机 AMKR 服务的统一入口、用量与最近趋势。")),
      h("div.spacer"),
      store.health?.base_url
        ? buttonNode(`复制地址 ${store.health.base_url}`, {
            variant: "secondary", small: true,
            onClick: () => copyText(store.health.base_url).then(() => toast("地址已复制")),
          })
        : null,
    ),
    store.health && !store.health.local_auth_enabled
      ? notice("本地鉴权未启用：管理接口对本机开放。建议在设置中启用。", "warn")
      : null,
    h("div.grid.wide", {}, unifiedCard(xtx), metricsCard(xtx)),
    trendCard(xtx),
  );
}
