// 登录页：管理面的**独立**凭据入口（webui/login.html）。
//
// 为什么单独一页而不是主界面里的浮层：主界面（index.html）只在鉴权通过后才渲染，
// 未通过时它整页无处可画。把登录做成"未授权时就地画一张卡"，等于同一个地址既是主
// 界面又是登录框——深链、前进后退、"会话中途失效"三件事都得在这一页里兼顾，而每条
// 路径都要重新证明"没有绕过鉴权的入口"。拆成两个页面后两边各自只有一件事：
//
//   index.html —— 只画主界面。发现没有可用凭据就整页 replace 到登录页，并把用户原本
//                 要去的地址放进 ?next=；
//   login.html —— 只收凭据。验通了再 replace 回 ?next=。
//
// 用 replace 而不是 href：登录页不该留在历史记录里，否则"后退"会退回到一个已经有
// 凭据的表单。
//
// 三条边界：
//   1. **凭据不进 URL**。Key 只写 localStorage（`amkr.apiKey`，见 api.js）。登录页地址
//      会进历史记录、Referer 与截图，把 Key 放进去等于泄漏。
//   2. **只接受同源 next**（见 safeNext）。否则 `?next=//evil.example` 就是一个开放
//      重定向：用户在本站输入了真实 Key，却被送到站外。
//   3. **未经服务端验证的 Key 不留在 localStorage**。留下的话，下次打开主界面会拿着
//      这把无效凭据打一轮 401 才回到这里。
//
// 输入框节点全程复用（paint 只重绘提示、重试按钮与页脚，从不重建输入框）：健康轮询
// 会周期性重绘提示，重建会把用户已经粘了一半的 Key 与光标一起清掉。

import { h, errorText } from "../dom.js";
import { api, apiBase, setKey, ApiError } from "../api.js";
import { notice, buttonNode, input, field, render } from "../ui.js";
import { icon } from "../icons.js";

const HEALTH_INTERVAL = 5000;
const KEY_HINT = "可在终端执行 amkr --show-api-key 获取本地鉴权 Key。";
const KEY_INVALID = "本地鉴权 Key 无效，请重新核对后重试。可在终端执行 amkr --show-api-key 获取。";
const KEY_EXPIRED = "本地鉴权 Key 已失效，请重新输入。";

const state = {
  health: null,
  connectionError: null,
  // { text, tone }：会话失效（?reason=expired）、输入为空、Key 无效三种提示都走这里。
  // 与 connectionError 分开：前者是"凭据/输入的问题"，后者是"服务没连上"，两者同时
  // 成立时该显示前者——重试连接解决不了 Key 打错。
  notice: null,
  busy: false,
};

let host = null;
let keyInput = null;
let statusHost = null;
let retryHost = null;
let footHost = null;
let connectBtn = null;

// safeNext 只放行同源相对路径。
//
// 绝对地址（`https://…`）与协议相对地址（`//host`）都会被浏览器当成站外跳转，而
// `?next=` 是攻击者完全可控的输入——一条 `…/login.html?next=//evil.example` 的链接
// 就能让用户在本站输入真实 Key 之后被送去站外。反斜杠变体（`/\evil.example`）在特殊
// 协议下与 `//` 等价，一并拒绝。
//
// 先按浏览器的方式归一化再校验，顺序不能反：浏览器在解析 URL 前会**剥掉**制表符与
// 换行（ASCII tab/LF/CR），于是 `"/\t/evil.example"` 在它眼里就是 `"//evil.example"`。
// 若直接校验原始串，第二个字符是 `\t` 而不是 `/`，看着人畜无害，交给 location.replace
// 却会变成站外跳转——校验的和使用必须对着同一个字符串。
function safeNext(raw) {
  const value = String(raw || "").replace(/[\t\n\r]/g, "").trim();
  if (!value.startsWith("/")) return null;
  if (/^\/[/\\]/.test(value)) return null;
  return value;
}

// appURL 是管理面首页，也是没有可信 next 时的落点。
// 与 api.js 的 apiBase 同源：独立运行是 /ui/，嵌入宿主是 /amkr/ui/。
function appURL() {
  return `${apiBase()}/ui/`;
}

// enterApp 验通之后回到用户原本要去的地址。next 不可信（或没带）就回首页。
function enterApp() {
  const next = safeNext(new URLSearchParams(location.search || "").get("next"));
  location.replace(next || appURL());
}

export async function bootLogin() {
  const root = document.getElementById("root");
  if (!root) return;

  const params = new URLSearchParams(location.search || "");
  // 主界面在会话中途收到 401 时带上 reason=expired：同一个登录页要能区分"从没登录过"
  // （给获取 Key 的提示）与"原来那把失效了"（给失效说明）。
  if (params.get("reason") === "expired") state.notice = { text: KEY_EXPIRED, tone: "warn" };

  // 先探服务再决定画不画表单。反过来的话，关掉本地鉴权的部署会先闪一下登录框再跳走
  // ——用户看到的是一次自己没触发的"登录"。
  if (await refresh()) return;

  keyInput = input({ type: "password", placeholder: "粘贴本地鉴权 Key", autocomplete: "off" });
  keyInput.addEventListener("keydown", (event) => { if (event.key === "Enter") submit(); });
  statusHost = h("div");
  retryHost = h("div.btn-row");
  footHost = h("div.login-foot");
  connectBtn = buttonNode("连接", { onClick: () => submit(), iconName: "key" });

  host = h("div.login-shell", {},
    h("div.card.login-card", {},
      h("div.login-head", {},
        h("span.login-mark", {}, icon("shield", { size: 24 })),
        h("div", {}, h("h2", "连接到 AMKR"), h("p.muted", "管理接口需要本地鉴权 Key")),
      ),
      h("div.stack", {}, statusHost, field("本地鉴权 Key", keyInput), retryHost),
      h("div.btn-row", { style: { marginTop: "16px" } }, connectBtn),
      footHost,
    ),
  );
  root.append(host);
  paint();
  setInterval(refresh, HEALTH_INTERVAL);
}

// refresh 重新探一次服务状态。
//
// 与主界面一样，连得上但不需要鉴权（local_auth_enabled=false）时登录页没有要问的东西，
// 直接放行——否则部署关掉本地鉴权后会卡在一个永远填不对的表单上。
//
// 返回值表示"已放行、本页收工"：调用方据此停下，不再画表单。
async function refresh() {
  try {
    state.health = await api.health();
    state.connectionError = null;
  } catch (error) {
    state.health = null;
    state.connectionError = errorText(error);
  }
  if (state.health && !state.health.local_auth_enabled) { enterApp(); return true; }
  paint();
  return false;
}

// verify 用待验证的 Key 打一次真实请求。
//
// 顺序只能是"先写 localStorage 再验"——api.js 是从 localStorage 取 Key 的。因此失败
// 路径必须显式清掉（调用方负责），不然无效 Key 会留在本机。
async function verify(value) {
  setKey(value);
  if (!state.health) await refresh();
  try {
    await api.settings();
    state.connectionError = null;
    return "ok";
  } catch (error) {
    if (error instanceof ApiError && error.isUnauthorized) return "unauthorized";
    // 连不上与 Key 打错是两回事：前者要重试，后者要改 Key，文案与按钮都不同。
    state.connectionError = errorText(error);
    return "unreachable";
  }
}

async function submit() {
  const value = keyInput.value.trim();
  if (!value) {
    state.notice = { text: "请先填写本地鉴权 Key。", tone: "warn" };
    paint();
    return;
  }
  // 清掉上一次的提示，否则"先填 Key"之类的旧提示会盖住本次的真实结果。
  state.notice = null;
  state.busy = true;
  paint();
  const status = await verify(value);
  state.busy = false;
  if (status === "ok") { enterApp(); return; }
  if (status === "unauthorized") { setKey(""); state.notice = { text: KEY_INVALID, tone: "error" }; }
  paint();
}

// retry 处理"服务没起来"：还没填 Key 时就只重探服务，不该拿空凭据去验一遍再报
// "Key 无效"（那是把服务故障说成了用户填错）。
async function retry() {
  const value = keyInput.value.trim();
  state.connectionError = null;
  if (!value) { await refresh(); return; }
  state.busy = true;
  paint();
  const status = await verify(value);
  state.busy = false;
  if (status === "ok") { enterApp(); return; }
  // 服务恢复后才发现 Key 也失效了，同样要清掉并说明原因。
  if (status === "unauthorized") { setKey(""); state.notice = { text: KEY_INVALID, tone: "error" }; }
  paint();
}

// paint 只动提示区、重试区与页脚，**不重建输入框**（见文件头说明）。
function paint() {
  if (!host) return;
  const failure = state.connectionError;
  render(statusHost,
    state.notice
      ? notice(state.notice.text, state.notice.tone)
      : failure
        ? notice(`无法连接 AMKR 服务：${failure}`, "error")
        : notice(KEY_HINT, "info"),
  );
  // 已经拿不到服务时才给重试入口；Key 本身错了要改 Key，重试没有意义。
  render(retryHost, failure && !state.notice
    ? buttonNode("重试连接", {
        variant: "secondary",
        iconName: "refresh",
        onClick: () => retry(),
      })
    : null);
  render(footHost, footText());
  connectBtn.disabled = state.busy;
}

// 独立页没有导航栏，把版本与入口放在页脚：能一眼确认连的是哪个实例。
function footText() {
  const health = state.health;
  return [
    health?.version ? `版本 v${health.version}` : null,
    health?.base_url || null,
  ].filter(Boolean).join(" · ") || "AMKR WebUI";
}
