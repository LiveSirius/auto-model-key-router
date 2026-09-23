// 访客看板的客户端。
//
// 「访客」= 访问密钥（amkr_ak_…）的持有者。与 panel-api.js 的关系：那个服务**工作
// 空间面板**（嵌入方的空间 key，能读写自己空间的任务），这个只服务**只读的用量
// 看板**（这把 key 自己用了多少）。两者刻意分开而不是合成一个客户端：
//
//   - 凭据来源不同。面板 key 从 URL fragment 来（嵌入方拼好的地址）；访客是自己
//     打开页面、粘贴自己的 key，因此凭据存 localStorage（见下）。
//   - 权限面不同。面板能写任务；访客**只有读**，一个写接口都不发。
//
// # 凭据存放：为什么这里可以用 localStorage，而 panel-api.js 严禁
//
// panel-api.js 的禁令是为**嵌入方**设的：面板页与 AMKR 管理面常常同源，用同一份
// localStorage 会让面板 key 与管理员的本地鉴权 Key 互相覆盖。访客看板是**独立入口**
// （自己的 HTML 页），不与任何宿主共享存储，因此没有这个冲突。
//
// 但仍有两条硬约束：
//   1. 键名与 api.js 的 amkr.apiKey **不同**（见 GUEST_KEY）。共用键名会让访客的
//      访问密钥覆盖管理员已登录的凭据，管理员回到管理面就掉线。
//   2. 无论存的是哪一把，服务端都只按这把 key 自己的清单授权——前端存哪里不构成
//      权限边界，真正的边界在 /ui/access-key-usage.json 的鉴权里。

import { apiBase, ApiError } from "./api.js";

// GUEST_KEY 是 localStorage 里的访客凭据键名。
//
// 刻意与 api.js 的 "amkr.apiKey" 不同：同一个浏览器可能既有管理员登录、又要看访客
// 看板（例如运维自己兼一个试用账号），共用键名会让两者互相踢下线。
const GUEST_KEY = "amkr.guestAccessKey";

// guestCredential 读回已保存的访客凭据（没有时返回空串）。
export function guestCredential() {
  try {
    return globalThis.localStorage?.getItem(GUEST_KEY) || "";
  } catch {
    // 隐私模式下 localStorage 可能直接抛异常。取不到就当没存过，让人重新填。
    return "";
  }
}

// saveGuestCredential 记住访客凭据；空串表示清除。
export function saveGuestCredential(key) {
  try {
    if (key) globalThis.localStorage?.setItem(GUEST_KEY, key);
    else globalThis.localStorage?.removeItem(GUEST_KEY);
  } catch {
    // 存不下不影响本次会话：调用方手上仍有 key。
  }
}

// guestRequest 发一条访客请求。
//
// 与 api.js 的 request 刻意分开（那是管理面的、会读 amkr.apiKey）：
//   - 凭据显式传入，绝不隐式去读管理面的 key；
//   - 401 不做全局跳转（访客看板没有"登录卡"以外的状态需要维护）；
//   - 不发 X-AMKR-Workspace：可见范围完全由 key 决定，多发一个头只会让读代码的人
//     以为换个头就能换范围。
export async function guestRequest(key, path, { method = "GET" } = {}) {
  const headers = {};
  // 空 key 表示这个端点不鉴权（价格目录）。不发一个 `Bearer ` 的空头：那会让
  // 服务端把它当成"提供了但无效的凭据"，而不是"没提供"。
  if (key) headers.Authorization = `Bearer ${key}`;
  let response;
  try {
    response = await fetch(`${apiBase()}${path}`, { method, headers });
  } catch (error) {
    throw new ApiError(`无法连接 AMKR 服务: ${error.message}`, 0, null);
  }
  const text = await response.text();
  let payload = null;
  if (text) {
    try {
      payload = JSON.parse(text);
    } catch {
      payload = text;
    }
  }
  if (!response.ok) {
    // 服务端的错误信封是 {"error":{"message":...}}；面板面的是 {"detail":...}。
    // 两种都认，好让后端换一种写法时这里不会退化成 "HTTP 500"。
    let detail = `HTTP ${response.status}`;
    if (payload && typeof payload === "object") {
      detail = payload?.error?.message || payload?.detail || detail;
    }
    throw new ApiError(String(detail), response.status, detail);
  }
  return payload;
}

// createGuestApi 绑定一份凭据，给出看板需要的全部调用。
//
// 只有一个端点：访客看板是**只读**的，任何写操作都不属于它。把清单收在这里而不是
// 散在页面里，是为了让「这一页能做什么」一眼可数。
export function createGuestApi(key) {
  return {
    usage: ({ hours = 24, allHistory = false } = {}) =>
      guestRequest(key, allHistory
        ? "/ui/access-key-usage.json?all_history=true"
        : `/ui/access-key-usage.json?hours=${encodeURIComponent(hours)}`),
    // 价格目录不鉴权（内容是 models.dev 的公开数据），因此不走 guestRequest 的
    // Authorization——但走同一个 base，这样子路径挂载下也对。
    pricing: () => guestRequest("", "/ui/pricing.json"),
  };
}
