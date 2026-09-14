// amkr WebUI —— 与路由服务通信的唯一入口。
// 除 /health 外，所有管理接口都需要本地鉴权 Key（Authorization: Bearer）。

const KEY_STORAGE = "amkr.apiKey";

export function getKey() {
  return localStorage.getItem(KEY_STORAGE) || "";
}

export function setKey(value) {
  if (value) localStorage.setItem(KEY_STORAGE, value);
  else localStorage.removeItem(KEY_STORAGE);
}

export class ApiError extends Error {
  constructor(message, status, detail) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.detail = detail;
  }
  get isConflict() {
    return this.status === 409;
  }
  get isUnauthorized() {
    return this.status === 401;
  }
}

function detailText(payload, status) {
  if (payload && typeof payload === "object") {
    const d = payload.detail;
    if (typeof d === "string") return d;
    if (Array.isArray(d) && d.length) {
      return d.map((item) => item.msg || JSON.stringify(item)).join("；");
    }
    if (d) return JSON.stringify(d);
  }
  return `HTTP ${status}`;
}

async function request(path, { method = "GET", body, auth = true } = {}) {
  const headers = {};
  if (auth) headers.Authorization = `Bearer ${getKey()}`;
  if (body !== undefined) headers["Content-Type"] = "application/json";
  let response;
  try {
    response = await fetch(path, {
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
    const detail = detailText(payload, response.status);
    throw new ApiError(`AMKR 请求失败（HTTP ${response.status}）: ${detail}`, response.status, detail);
  }
  return payload;
}

export const api = {
  health: () => request("/health", { auth: false }),
  metrics: (hours = 1) => request(`/metrics?hours=${hours}`),
  series: (hours = 1, bucketSeconds = 60) =>
    request(`/metrics/series?hours=${hours}&bucket_seconds=${bucketSeconds}`),
  logs: () => request("/api/logs"),

  tool: () => request("/api/tool"),
  setWebui: (enabled) => request("/api/tool/webui", { method: "POST", body: { enabled } }),
  runService: (action) => request(`/api/service/${action}`, { method: "POST" }),

  integrations: () => request("/api/integrations"),
  applyIntegration: (agent, mode) =>
    request(`/api/integrations/${agent}`, { method: "POST", body: { mode } }),
  rollbackIntegration: (agent) =>
    request(`/api/integrations/${agent}/rollback`, { method: "POST" }),

  settings: () => request("/api/settings"),
  updateSettings: (revision, settings) =>
    request("/api/settings", { method: "PUT", body: { config_revision: revision, ...settings } }),
  regenerateLocalKey: (revision) =>
    request("/api/settings/local-api-key", { method: "POST", body: { config_revision: revision } }),
  checkUpdate: () => request("/api/update/check", { method: "POST" }),

  exportConfig: () => request("/api/config/export", { method: "POST" }),
  importConfig: (revision, config) =>
    request("/api/config/import", { method: "POST", body: { config_revision: revision, config } }),

  providers: () => request("/api/providers"),
  createProvider: (revision, id, baseUrl) =>
    request("/api/providers", { method: "POST", body: { config_revision: revision, id, base_url: baseUrl } }),
  updateProvider: (revision, providerId, id, baseUrl, routes) =>
    request(`/api/providers/${encodeURIComponent(providerId)}`, {
      method: "PUT",
      body: { config_revision: revision, id, base_url: baseUrl, routes },
    }),
  deleteProvider: (revision, providerId) =>
    request(`/api/providers/${encodeURIComponent(providerId)}`, {
      method: "DELETE",
      body: { config_revision: revision },
    }),

  createProviderKey: (revision, providerId, name, apiKey, allowVisitor) =>
    request(`/api/providers/${encodeURIComponent(providerId)}/keys`, {
      method: "POST",
      body: { config_revision: revision, name, api_key: apiKey, allow_visitor: allowVisitor },
    }),
  updateProviderKey: (revision, providerId, keyName, patch) =>
    request(`/api/providers/${encodeURIComponent(providerId)}/keys/${encodeURIComponent(keyName)}`, {
      method: "PUT",
      body: { config_revision: revision, ...patch },
    }),
  deleteProviderKey: (revision, providerId, keyName) =>
    request(`/api/providers/${encodeURIComponent(providerId)}/keys/${encodeURIComponent(keyName)}`, {
      method: "DELETE",
      body: { config_revision: revision },
    }),

  keyModels: (providerId, keyName) =>
    request(`/api/providers/${encodeURIComponent(providerId)}/keys/${encodeURIComponent(keyName)}/models`),
  setKeyModels: (revision, providerId, keyName, models) =>
    request(`/api/providers/${encodeURIComponent(providerId)}/keys/${encodeURIComponent(keyName)}/models`, {
      method: "PUT",
      body: { config_revision: revision, models },
    }),

  probeProvider: (revision, providerId) =>
    request(`/api/providers/${encodeURIComponent(providerId)}/probe`, {
      method: "POST",
      body: { config_revision: revision },
    }),
  probeKey: (revision, providerId, keyName) =>
    request(`/api/providers/${encodeURIComponent(providerId)}/keys/${encodeURIComponent(keyName)}/probe`, {
      method: "POST",
      body: { config_revision: revision },
    }),
  startProbe: (providerId, keys, timeoutSeconds) =>
    request("/api/probes/keys", {
      method: "POST",
      body: { provider_id: providerId, keys, timeout_seconds: timeoutSeconds },
    }),
  getProbe: (probeId) => request(`/api/probes/${encodeURIComponent(probeId)}`),
  cancelProbe: (probeId) =>
    request(`/api/probes/${encodeURIComponent(probeId)}/cancel`, { method: "POST" }),

  routes: () => request("/api/routes"),
  createRoute: (revision, id, targets, aliases, routingMode) =>
    request("/api/routes", {
      method: "POST",
      body: { config_revision: revision, id, targets, aliases, routing_mode: routingMode },
    }),
  updateRoute: (revision, routeId, targets, aliases, routingMode) =>
    request(`/api/routes/${encodeURIComponent(routeId)}`, {
      method: "PUT",
      body: { config_revision: revision, targets, aliases, routing_mode: routingMode },
    }),
  deleteRoute: (revision, routeId) =>
    request(`/api/routes/${encodeURIComponent(routeId)}`, {
      method: "DELETE",
      body: { config_revision: revision },
    }),

  models: () => request("/api/models"),
  updateModelEffort: (revision, modelId, effort) =>
    request(`/api/models/${encodeURIComponent(modelId)}`, {
      method: "PUT",
      body: { config_revision: revision, reasoning_effort: effort },
    }),

  unified: () => request("/api/unified-model"),
  updateUnified: (revision, unified) =>
    request("/api/unified-model", { method: "PUT", body: { config_revision: revision, default: unified.default, image: unified.image ?? null } }),
  deleteUnified: (revision) =>
    request("/api/unified-model", { method: "DELETE", body: { config_revision: revision } }),
};
