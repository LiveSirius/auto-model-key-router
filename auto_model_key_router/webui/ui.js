// 共享 UI 组件：对话框、提示条、Toast、状态徽标、数据表。
// 全部为无状态工厂函数，调用方负责重新渲染。

import { h, mount } from "./dom.js";

let toastHost = null;

export function installToastHost(root) {
  toastHost = h("div.toast-host", { role: "status", "aria-live": "polite" });
  root.append(toastHost);
}

export function toast(message, tone = "info") {
  if (!toastHost) return;
  const node = h("div.toast", tone === "error" ? { style: { background: "#b00020" } } : null, message);
  toastHost.append(node);
  setTimeout(() => node.remove(), 3200);
}

// —— 模态对话框 ——
// closeOnBackdrop=false 用于"操作进行中不允许关闭"的场景。
export function dialog({ title, body, actions = [], onClose, closeOnBackdrop = true }) {
  const backdrop = h("div.backdrop", { role: "presentation" });
  const close = () => {
    backdrop.remove();
    document.removeEventListener("keydown", onKey);
    if (onClose) onClose();
  };
  const onKey = (event) => {
    if (event.key === "Escape" && closeOnBackdrop) close();
  };
  const panel = h(
    "div.dialog",
    { role: "dialog", "aria-modal": "true", "aria-label": typeof title === "string" ? title : "对话框" },
    h("h3", title),
    h("div.dialog-body", body),
    actions.length ? h("div.dialog-actions", actions.map((action) => button(action))) : null,
  );
  if (closeOnBackdrop) {
    backdrop.addEventListener("click", (event) => { if (event.target === backdrop) close(); });
  }
  document.addEventListener("keydown", onKey);
  backdrop.append(panel);
  document.body.append(backdrop);
  const focusable = panel.querySelector("input, select, textarea, button");
  if (focusable) focusable.focus();
  return { close, panel };
}

function button(spec) {
  return h(
    `button.btn.${spec.variant || "text"}${spec.small ? ".small" : ""}`,
    { type: "button", disabled: spec.disabled, onClick: spec.onClick },
    spec.label,
  );
}

export function confirmDialog({ title = "请确认", message, confirmLabel = "确认", danger = false, onConfirm }) {
  const ref = dialog({
    title,
    body: h("p", message),
    actions: [
      { label: "取消", variant: "text", onClick: () => ref.close() },
      {
        label: confirmLabel,
        variant: danger ? "danger" : "",
        onClick: () => { ref.close(); onConfirm(); },
      },
    ],
  });
  return ref;
}

// —— 基础块 ——
export function notice(text, tone = "info") {
  if (!text) return null;
  return h(`div.notice.${tone}`, { role: tone === "error" ? "alert" : "status" }, text);
}

export function badge(text, tone = "muted", extra) {
  return h(`span.badge.${tone}`, extra, text);
}

export function card(...children) {
  return h("div.card", children);
}

export function cardHead(title, ...rest) {
  return h("div.card-head", h("h3", title), ...rest);
}

export function stat(label, value, hint) {
  return h("div.card.stat", h("div.label", label), h("div.value", value), hint ? h("div.hint", hint) : null);
}

export function empty(text) {
  return h("div.empty", text);
}

export function field(label, control) {
  return h("label.field", h("span", label), control);
}

export function input(props) {
  return h("input.input", props);
}

export function select(options, props = {}) {
  const node = h("select.select", props);
  for (const option of options) {
    const spec = typeof option === "string" ? { value: option, label: option } : option;
    node.append(h("option", { value: spec.value, selected: String(props.value ?? "") === String(spec.value), disabled: spec.disabled }, spec.label));
  }
  return node;
}

export function buttonNode(label, props = {}) {
  const { variant = "", small = false, ...rest } = props;
  return h(`button.btn.${variant}${small ? ".small" : ""}`, { type: "button", ...rest }, label);
}

export function toggle(label, pressed, onClick, props = {}) {
  return h("button.toggle", { type: "button", "aria-pressed": String(!!pressed), onClick, ...props }, label);
}

export function table(columns, rows, emptyText) {
  if (!rows.length) return empty(emptyText || "暂无数据。");
  const head = h("tr", columns.map((column) => h(`th${column.numeric ? ".num" : ""}`, column.label)));
  const body = rows.map((row) =>
    h("tr", columns.map((column) => {
      const value = column.render(row);
      return h(`td${column.numeric ? ".num" : ""}`, value === null || value === undefined ? "-" : value);
    })),
  );
  return h("table.table", h("thead", head), h("tbody", body));
}

export function kv(pairs) {
  const list = h("dl.kv");
  for (const [key, value] of pairs) {
    if (value === null || value === undefined) continue;
    list.append(h("dt", key), h("dd", value));
  }
  return list;
}

export function loading(text = "正在读取…") {
  return h("div.inline", h("span.spinner"), h("span.muted", text));
}

export function progressBar(percent) {
  const value = Math.max(0, Math.min(100, percent ?? 0));
  return h(
    "div",
    { style: { height: "4px", background: "#e0e0e0", borderRadius: "2px", overflow: "hidden" } },
    h("div", { style: { width: value + "%", height: "100%", background: "var(--md-primary)", transition: "width 250ms cubic-bezier(0.4,0,0.2,1)" } }),
  );
}

export function render(target, ...children) {
  mount(target, ...children);
}
