// SVG 图表层：折线（含十字准线读数）、堆叠柱、环形、迷你趋势、横向排行。
//
// 设计约束：
// - 零依赖、零构建，随包发布，离线可用；
// - 所有取值都走 chart-math.js 的口径函数，"画"和"算"分离，方便单测；
// - 图表以真实像素宽度渲染（不用 preserveAspectRatio="none" 拉伸），
//   因为拉伸会把描边和文字一起压扁，读数就不准了。

import { h, svg, formatClock, formatClockSeconds, formatDateTime, clamp } from "./dom.js";
import {
  METRIC_MAP, axisScale, timeTicks, metricValue, windowSums,
  formatNumber, formatCompactNumber, formatPercentValue, formatDurationValue,
} from "./chart-math.js";

// 图表内边距（px）。左侧留给 Y 轴刻度文字，底部留给时间标签。
const PAD = { top: 16, right: 16, bottom: 30, left: 60 };

// 统一的描边宽度与配色语义色名。
const TONE_VAR = {
  primary: "var(--md-primary)",
  secondary: "var(--md-secondary-variant)",
  error: "var(--md-error)",
  neutral: "var(--md-on-surface-variant)",
};

// 轴标签字体走 CSS（.chart .axis-label），不在这里写内联样式：
// SVG 表现属性不支持 var()，写成 font-family="var(--font)" 会被当作字面字族名而失效。
const AXIS_TEXT = { "font-size": "10" };

function sizing(host, draw) {
  // 用 ResizeObserver 跟随容器宽度重绘；探针环境没有它就直接按测量值画一次。
  const render = () => {
    const width = Math.max(240, Math.round(host.clientWidth || host.parentElement?.clientWidth || 720));
    host.replaceChildren(draw(width));
  };
  render();
  if (typeof ResizeObserver === "undefined") return host;
  let frame = 0;
  const observer = new ResizeObserver(() => {
    // 页面轮询会整块替换图表节点。观察已脱离文档的节点既能防止泄漏，
    // 也能顺手放掉 rAF，所以先判 isConnected 再排队重绘。
    if (!host.isConnected) { observer.disconnect(); return; }
    if (frame) return;
    frame = requestAnimationFrame(() => {
      frame = 0;
      if (!host.isConnected) { observer.disconnect(); return; }
      render();
    });
  });
  observer.observe(host);
  return host;
}

// 数值 → 展示文本，按指标类型分派。
function formatValue(metric, value) {
  if (value === null || value === undefined || !Number.isFinite(value)) return "—";
  if (metric.percent) return formatPercentValue(value);
  if (metric.unit === "ms") return formatDurationValue(value);
  return metric.format(value);
}

function withUnit(metric, value) {
  const text = formatValue(metric, value);
  if (text === "—") return text;
  if (metric.percent) return text;
  return metric.unit ? `${text} ${metric.unit}` : text;
}

// 把数据点切成"连续非空"的段；0 是有效读数，只有 null 才断线。
function segments(points) {
  const out = [];
  let current = null;
  points.forEach((point, index) => {
    if (point.value === null || point.value === undefined || !Number.isFinite(point.value)) {
      current = null;
      return;
    }
    if (!current) { current = []; out.push(current); }
    current.push(index);
  });
  return out;
}

// —— 折线 / 面积图 ——
export function lineChart({
  points,
  metricId = "rpm",
  bucketSeconds = 60,
  height = 240,
  showArea = true,
  ariaLabel,
}) {
  const metric = METRIC_MAP[metricId] || METRIC_MAP.rpm;
  const host = h("div.chart-host", { style: { height: `${height}px` } });
  const series = (points || []).map((point, index) => ({
    index,
    at: point.started_at,
    endedAt: point.ended_at,
    complete: point.complete !== false,
    value: metricValue(point, metric, bucketSeconds),
    raw: point,
  }));

  sizing(host, (width) => {
    const innerW = Math.max(10, width - PAD.left - PAD.right);
    const innerH = Math.max(10, height - PAD.top - PAD.bottom);
    const scale = axisScale(series.map((point) => point.value), { percent: metric.percent });
    const count = series.length;
    const step = count > 1 ? innerW / (count - 1) : 0;
    const x = (index) => (count === 1 ? PAD.left + innerW / 2 : PAD.left + index * step);
    const y = (value) => PAD.top + innerH - ((clamp(value, scale.min, scale.max) - scale.min) / (scale.max - scale.min || 1)) * innerH;

    const node = svg("svg", {
      width, height, viewBox: `0 0 ${width} ${height}`,
      class: "chart", role: "img",
      "aria-label": ariaLabel || `${metric.label}趋势图`,
    });

    // 横向网格 + Y 轴刻度。percent 恒为 0-100，其余按 nice 阶梯。
    for (const tick of scale.ticks) {
      const ty = y(tick);
      node.append(svg("line", {
        class: "grid-line", x1: PAD.left, x2: width - PAD.right, y1: ty, y2: ty,
      }));
      node.append(svg("text", {
        class: "axis-label", x: PAD.left - 8, y: ty + 3.5, "text-anchor": "end", ...AXIS_TEXT,
      }, metric.percent ? `${Math.round(tick)}%` : formatCompactNumber(tick)));
    }

    // 时间轴标签
    for (const tick of timeTicks(series, Math.max(2, Math.floor(innerW / 92)))) {
      node.append(svg("text", {
        class: "axis-label", x: x(tick.index), y: height - PAD.bottom + 18,
        "text-anchor": "middle", ...AXIS_TEXT,
      }, formatClock(tick.at)));
    }

    const baseline = PAD.top + innerH;
    const runs = segments(series);

    for (const run of runs) {
      const coords = run.map((index) => [x(index), y(series[index].value)]);
      const line = coords.map(([px, py]) => `${px.toFixed(2)},${py.toFixed(2)}`).join(" ");
      if (showArea && coords.length > 1) {
        const first = coords[0];
        const last = coords[coords.length - 1];
        node.append(svg("path", {
          class: "area",
          d: `M ${first[0].toFixed(2)},${baseline.toFixed(2)} L ${line.replace(/ /g, " L ")} L ${last[0].toFixed(2)},${baseline.toFixed(2)} Z`,
        }));
      }
      if (coords.length === 1) {
        node.append(svg("circle", { class: "point", cx: coords[0][0], cy: coords[0][1], r: 2.6 }));
      } else {
        node.append(svg("polyline", { class: "series-line", points: line }));
        // 仍在累加中的尾桶用虚线：它天然偏低，不能被读成流量骤降。
        const lastIndex = run[run.length - 1];
        if (series[lastIndex]?.complete === false && run.length > 1) {
          const prev = coords[coords.length - 2];
          const last = coords[coords.length - 1];
          node.append(svg("polyline", {
            class: "series-line is-partial",
            points: `${prev[0].toFixed(2)},${prev[1].toFixed(2)} ${last[0].toFixed(2)},${last[1].toFixed(2)}`,
          }));
        }
      }
    }

    // 交互层：十字准线 + 最近点高亮 + 读数气泡
    const crosshair = svg("line", { class: "crosshair", y1: PAD.top, y2: PAD.top + innerH, x1: 0, x2: 0, opacity: 0 });
    const marker = svg("circle", { class: "marker", r: 4.5, cx: 0, cy: 0, opacity: 0 });
    node.append(crosshair, marker);

    const overlay = svg("rect", {
      x: PAD.left - step / 2, y: PAD.top, width: Math.max(innerW + step, 1), height: innerH,
      fill: "transparent", style: { cursor: "crosshair" },
    });
    node.append(overlay);

    const tooltip = h("div.chart-tip", { role: "status" });
    const frame = h("div.chart-frame", {}, node, tooltip);

    const showAt = (index) => {
      const point = series[index];
      if (!point || point.value === null) { hide(); return; }
      const px = x(index);
      const py = y(point.value);
      crosshair.setAttribute("x1", px); crosshair.setAttribute("x2", px); crosshair.setAttribute("opacity", 1);
      marker.setAttribute("cx", px); marker.setAttribute("cy", py); marker.setAttribute("opacity", 1);
      tooltip.replaceChildren(
        h("span.tip-time", formatDateTime(point.at)),
        h("span.tip-value", withUnit(metric, point.value)),
        point.complete === false ? h("span.tip-flag", "累加中") : null,
      );
      tooltip.classList.add("is-visible");
      // 气泡跟随但夹在容器内，避免右端溢出被裁。
      const tipWidth = tooltip.offsetWidth || 150;
      const left = clamp(px - tipWidth / 2, 4, Math.max(4, width - tipWidth - 4));
      tooltip.style.left = `${left}px`;
      tooltip.style.top = `${clamp(py - 46, 4, height - 20)}px`;
    };
    const hide = () => {
      crosshair.setAttribute("opacity", 0);
      marker.setAttribute("opacity", 0);
      tooltip.classList.remove("is-visible");
    };

    overlay.addEventListener("pointermove", (event) => {
      const rect = node.getBoundingClientRect();
      const ratio = rect.width ? (event.clientX - rect.left) / rect.width : 0;
      const localX = ratio * width;
      const index = count <= 1 ? 0 : Math.round((localX - PAD.left) / (step || 1));
      showAt(clamp(index, 0, count - 1));
    });
    overlay.addEventListener("pointerleave", hide);

    if (!series.some((point) => point.value !== null)) {
      frame.append(h("div.chart-empty", "该时间窗内没有请求"));
    }
    return frame;
  });
  return host;
}

// —— 堆叠柱：输入 / 输出 Token 随时间构成 ——
export function stackedBars({ points, height = 200, series: seriesSpec, bucketSeconds = 60 }) {
  const specs = seriesSpec || [
    { id: "prompt", label: "输入", tone: "primary", pick: (point) => point.prompt_tokens },
    { id: "completion", label: "输出", tone: "secondary", pick: (point) => point.completion_tokens },
  ];
  const host = h("div.chart-host", { style: { height: `${height}px` } });

  sizing(host, (width) => {
    const innerW = Math.max(10, width - PAD.left - PAD.right);
    const innerH = Math.max(10, height - PAD.top - PAD.bottom);
    const totals = (points || []).map((point) =>
      specs.reduce((sum, spec) => sum + (Number(spec.pick(point)) || 0), 0));
    const scale = axisScale(totals, { ticks: 4 });
    const count = (points || []).length;
    const slot = count ? innerW / count : innerW;
    // 1px 间隙保证相邻柱不会糊成一片色块。
    const barWidth = Math.max(1, slot - Math.max(1, slot * 0.18));

    const node = svg("svg", {
      width, height, viewBox: `0 0 ${width} ${height}`,
      class: "chart stacked", role: "img", "aria-label": "Token 构成随时间变化",
    });

    for (const tick of scale.ticks) {
      const ty = PAD.top + innerH - (tick / (scale.max || 1)) * innerH;
      node.append(svg("line", { class: "grid-line", x1: PAD.left, x2: width - PAD.right, y1: ty, y2: ty }));
      node.append(svg("text", {
        class: "axis-label", x: PAD.left - 8, y: ty + 3.5, "text-anchor": "end",
        ...AXIS_TEXT,
      }, formatCompactNumber(tick)));
    }

    const labelPoints = (points || []).map((point) => ({ started_at: point.started_at }));
    for (const tick of timeTicks(labelPoints, Math.max(2, Math.floor(innerW / 92)))) {
      node.append(svg("text", {
        class: "axis-label", x: PAD.left + tick.index * slot + barWidth / 2, y: height - PAD.bottom + 18,
        "text-anchor": "middle", ...AXIS_TEXT,
      }, formatClock(tick.at)));
    }

    const baseline = PAD.top + innerH;
    const tip = h("div.chart-tip", { role: "status" });
    const frame = h("div.chart-frame", {}, node, tip);

    (points || []).forEach((point, index) => {
      let cursor = baseline;
      const values = specs.map((spec) => ({ spec, value: Number(spec.pick(point)) || 0 }));
      for (const { spec, value } of values) {
        if (value <= 0) continue;
        const barHeight = (value / (scale.max || 1)) * innerH;
        cursor -= barHeight;
        const rect = svg("rect", {
          class: "bar", x: PAD.left + index * slot, y: cursor,
          width: barWidth, height: Math.max(barHeight, 0.5),
          fill: TONE_VAR[spec.tone] || TONE_VAR.primary,
        });
        rect.addEventListener("pointerenter", () => {
          const total = values.reduce((sum, item) => sum + item.value, 0);
          tip.replaceChildren(
            h("span.tip-time", formatDateTime(point.started_at)),
            ...values.map(({ spec: inner, value: innerValue }) =>
              h("span.tip-value", `${inner.label} ${formatCompactNumber(innerValue)}`)),
            h("span.tip-total", `合计 ${formatCompactNumber(total)}`),
            point.complete === false ? h("span.tip-flag", "累加中") : null,
          );
          tip.classList.add("is-visible");
          const tipWidth = tip.offsetWidth || 150;
          tip.style.left = `${clamp(PAD.left + index * slot - tipWidth / 2, 4, Math.max(4, width - tipWidth - 4))}px`;
          tip.style.top = `${PAD.top}px`;
        });
        rect.addEventListener("pointerleave", () => tip.classList.remove("is-visible"));
        node.append(rect);
      }
    });

    if (!totals.some((value) => value > 0)) frame.append(h("div.chart-empty", "该时间窗内没有 Token 用量"));
    return frame;
  });
  return host;
}

// —— 环形图：比率型指标的单一读数 ——
export function donut({ ratio, label, caption, size = 168, tone = "primary", emptyText = "无数据" }) {
  const value = Number.isFinite(ratio) ? clamp(ratio, 0, 1) : null;
  const stroke = 14;
  const radius = (size - stroke) / 2;
  const circumference = 2 * Math.PI * radius;
  const center = size / 2;

  const track = svg("circle", {
    cx: center, cy: center, r: radius, fill: "none",
    stroke: "var(--md-outline)", "stroke-width": stroke,
  });
  const arc = svg("circle", {
    cx: center, cy: center, r: radius, fill: "none",
    stroke: TONE_VAR[tone] || TONE_VAR.primary, "stroke-width": stroke,
    "stroke-linecap": "round",
    "stroke-dasharray": `${(value ?? 0) * circumference} ${circumference}`,
    transform: `rotate(-90 ${center} ${center})`,
  });
  const node = svg("svg", {
    width: size, height: size, viewBox: `0 0 ${size} ${size}`,
    class: "donut", role: "img",
    "aria-label": `${label} ${value === null ? emptyText : formatPercentValue(value)}`,
  }, track, arc);

  return h("div.donut-wrap", {},
    node,
    h("div.donut-center", {},
      h("strong", value === null ? emptyText : formatPercentValue(value, 1)),
      h("span", label),
    ),
    caption ? h("p.donut-caption", caption) : null,
  );
}

// —— 迷你趋势（KPI 瓦片内联）——
export function sparkline({ points, metricId = "rpm", bucketSeconds = 60, width = 96, height = 28, tone = "primary" }) {
  const metric = METRIC_MAP[metricId] || METRIC_MAP.rpm;
  const values = (points || []).map((point) => metricValue(point, metric, bucketSeconds));
  if (!values.some((value) => Number.isFinite(value))) return h("div.spark-empty");

  const scale = axisScale(values, { percent: metric.percent });
  const count = values.length;
  const step = count > 1 ? width / (count - 1) : 0;
  const x = (index) => (count === 1 ? width / 2 : index * step);
  const y = (value) => height - 2 - ((clamp(value, scale.min, scale.max) - scale.min) / (scale.max - scale.min || 1)) * (height - 4);

  const node = svg("svg", {
    width, height, viewBox: `0 0 ${width} ${height}`, class: `spark tone-${tone}`,
    "aria-hidden": "true", focusable: "false",
  });
  for (const run of segments(values.map((value, index) => ({ value, index })))) {
    const coords = run.map((index) => `${x(index).toFixed(1)},${y(values[index]).toFixed(1)}`);
    if (coords.length === 1) {
      node.append(svg("circle", { class: "point", cx: x(run[0]), cy: y(values[run[0]]), r: 1.6 }));
    } else {
      node.append(svg("polyline", { class: "spark-line", points: coords.join(" ") }));
    }
  }
  return node;
}

// —— 横向排行：把"谁在消耗"读成一句话 ——
export function barList(rows, { format = (row) => formatNumber(row.value, 0), tone = "primary", emptyText = "暂无数据" } = {}) {
  if (!rows.length) return h("div.bar-list-empty", emptyText);
  const max = Math.max(...rows.map((row) => row.value), 1);
  const list = h("div.bar-list", { role: "list" });
  for (const row of rows) {
    list.append(h("div.bar-row", { role: "listitem" },
      h("div.bar-head", {},
        h("span.bar-name", { title: row.name }, row.name),
        h("span.bar-value", format(row)),
      ),
      h("div.bar-track", {}, h("div", {
        class: `bar-fill tone-${tone}`,
        style: { width: `${Math.max(2, (row.value / max) * 100)}%` },
      })),
    ));
  }
  return list;
}

// —— 图例 ——
export function legend(items) {
  return h("div.legend", {}, items.map((item) =>
    h("span.legend-item", {},
      h("i", { class: `swatch tone-${item.tone || "primary"}` }),
      h("span", item.label),
      item.value !== undefined ? h("strong", item.value) : null,
    )));
}

// —— 窗口摘要（图表右上角的读数区）——
export function chartSummary({ points, metricId, bucketSeconds, windowHours }) {
  const metric = METRIC_MAP[metricId] || METRIC_MAP.rpm;
  const sums = windowSums(points || []);
  const summary = {
    ...sums,
    hours: windowHours || 0,
    // 速率口径：窗口总量 / 实际小时数换算到每分钟。
    requests: sums.requests,
  };
  const total = metric.total(summary);
  const latest = [...(points || [])].reverse()
    .map((point) => metricValue(point, metric, bucketSeconds))
    .find((value) => value !== null && value !== undefined);
  return { total, latest, sums, metric };
}

// 复用的格式化出口，避免页面各自拼字符串导致口径不一。
export const formatters = { formatNumber, formatCompactNumber, formatPercentValue, formatDurationValue, formatClockSeconds };
