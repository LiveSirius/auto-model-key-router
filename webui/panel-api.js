// 面板客户端：与宿主「只共享 origin、不共享任何存储」的 API 客户端。
//
// 面板可能被嵌在**任意**宿主页面里，而它与 AMKR 管理面常常同源（都是同一个服务发
// 出来的）。因此有一条硬约束：
//
//   **绝不读写 localStorage**。
//
// api.js 用 localStorage 的 amkr.apiKey 记住管理面的本地鉴权 Key。面板若顺手用它，
// 就会出现两种事故：面板把宿主管理员已登录的 Key 覆盖成面板 key（管理员回管理面就
// 掉线），或者反过来面板拿到管理员 Key——那就等于把整个实例的管理权限交给一个只该
// 管自己空间的嵌入页。面板的 key 只从 URL fragment 来，只存在内存里。
//
// key 走 fragment（#）而不是查询串：fragment 不会进 Referer、不进服务端访问日志，
// 而查询串两处都会留下明文凭据。

import { apiBase, ApiError } from "./api.js";

// 从 URL fragment 解析面板凭据与接口地址。
//
// 约定的 fragment 形状与 hash 路由同一套语法（#k=...&api=...），这样浏览器不会把它
// 当成路径发给服务端。没有 k 时返回空 key，由调用方给出"未提供凭据"的界面而不是
// 直接打接口——那样用户看到的会是 401，误以为 key 错了。
export function panelCredential(hash = location.hash) {
  const params = new URLSearchParams(String(hash).replace(/^#/, ""));
  const key = (params.get("k") || "").trim();
  // api 允许宿主指定 AMKR 的基址（面板页与接口不同源时），默认按页面路径反推。
  const api = (params.get("api") || "").replace(/\/+$/, "");
  return { key, base: api || apiBase() };
}

// panelRequest 发一条面板请求。
//
// 与 api.js 的 request 刻意分开而不是共用：那个函数会在 401 时调用全局
// onUnauthorized（把管理面踢回登录卡），面板不该有这个副作用。
export async function panelRequest(credential, path, { method = "GET", body } = {}) {
  const headers = { Authorization: `Bearer ${credential.key}` };
  if (body !== undefined) headers["Content-Type"] = "application/json";
  let response;
  try {
    response = await fetch(`${credential.base}${path}`, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch (error) {
    throw new ApiError(`无法连接 AMKR 服务: ${error.message}`, 0, null);
  }
  if (response.status === 204) return null;
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
    let detail = `HTTP ${response.status}`;
    if (payload && typeof payload === "object" && payload.detail) {
      detail = typeof payload.detail === "string" ? payload.detail : JSON.stringify(payload.detail);
    }
    throw new ApiError(`AMKR 请求失败（HTTP ${response.status}）: ${detail}`, response.status, detail);
  }
  return payload;
}

// createPanelApi 绑定一份凭据，给出面板需要的全部调用。
//
// 任务接口**不传 workspace**：空间由面板 key 决定，请求头指定空间在服务端会被忽略
// （见 internal/api/server.go 的 authorizedTaskConfig）。这里显式不传，免得读代码的
// 人以为换个头就能换空间。
export function createPanelApi(credential) {
  const call = (path, options) => panelRequest(credential, path, options);
  return {
    usage: ({ hours = 24, allHistory = false } = {}) =>
      call(allHistory
        ? "/ui/workspace-panel.json?all_history=true"
        : `/ui/workspace-panel.json?hours=${hours}`),
    tasks: () => call("/api/tasks"),
    createTask: (revision, payload) =>
      call("/api/tasks", { method: "POST", body: { config_revision: revision, ...payload } }),
    updateTask: (revision, taskName, payload) =>
      call(`/api/tasks/${encodeURIComponent(taskName)}`, {
        method: "PUT",
        body: { config_revision: revision, ...payload },
      }),
    deleteTask: (revision, taskName) =>
      call(`/api/tasks/${encodeURIComponent(taskName)}`, {
        method: "DELETE",
        body: { config_revision: revision },
      }),
  };
}
