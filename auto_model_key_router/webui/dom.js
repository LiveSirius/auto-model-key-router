// 极简 DOM 构建器：够用即可，避免为一个本地工具引入框架和构建步骤。

export function h(tag, props, ...children) {
  const [name, ...classes] = String(tag).split(".");
  const el = document.createElement(name || "div");
  if (classes.length) el.className = classes.join(" ");
  if (props && (typeof props !== "object" || props instanceof Node || Array.isArray(props))) {
    children.unshift(props);
    props = null;
  }
  for (const [key, value] of Object.entries(props || {})) {
    if (value === null || value === undefined || value === false) continue;
    if (key === "class") el.className = [el.className, value].filter(Boolean).join(" ");
    else if (key === "html") el.innerHTML = value;
    else if (key === "style" && typeof value === "object") Object.assign(el.style, value);
    else if (key.startsWith("on") && typeof value === "function") {
      el.addEventListener(key.slice(2).toLowerCase(), value);
    } else if (key === "value") el.value = value;
    else if (key === "checked" || key === "disabled" || key === "selected") el[key] = !!value;
    else el.setAttribute(key, value === true ? "" : value);
  }
  append(el, children);
  return el;
}

function append(el, children) {
  for (const child of children.flat(Infinity)) {
    if (child === null || child === undefined || child === false) continue;
    el.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
}

export function mount(target, ...children) {
  target.replaceChildren();
  append(target, children);
  return target;
}

export function clear(target) {
  target.replaceChildren();
}

// —— 数值格式化（与 Keyloom 展示口径一致）——
export function formatCount(value) {
  if (value === null || value === undefined) return "-";
  return Number(value).toLocaleString("zh-CN");
}

export function formatCompact(value) {
  if (value === null || value === undefined) return "-";
  const n = Number(value);
  if (Math.abs(n) >= 1e9) return trim(n / 1e9) + "B";
  if (Math.abs(n) >= 1e6) return trim(n / 1e6) + "M";
  if (Math.abs(n) >= 1e3) return trim(n / 1e3) + "K";
  return formatCount(n);
}

function trim(n) {
  return n.toFixed(1).replace(/\.0$/, "");
}

export function formatPercent(numerator, denominator) {
  if (!denominator) return "-";
  return Math.round((numerator / denominator) * 100) + "%";
}

export function formatRate(rate) {
  if (rate === null || rate === undefined) return "-";
  return Math.round(Number(rate) * 100) + "%";
}

export function formatDuration(ms) {
  if (ms === null || ms === undefined) return "-";
  return `${Math.round(Number(ms))}ms`;
}

export function shortRevision(revision) {
  return revision ? String(revision).slice(0, 8) : "读取中";
}

export function errorText(error) {
  return error && error.message ? error.message : String(error);
}

export function copyText(text) {
  if (navigator.clipboard && window.isSecureContext !== false) {
    return navigator.clipboard.writeText(text);
  }
  // 非安全上下文（http://局域网 IP）没有 clipboard API，退回 execCommand。
  const area = document.createElement("textarea");
  area.value = text;
  area.style.position = "fixed";
  area.style.opacity = "0";
  document.body.append(area);
  area.select();
  const ok = document.execCommand("copy");
  area.remove();
  return ok ? Promise.resolve() : Promise.reject(new Error("浏览器拒绝了复制操作"));
}
