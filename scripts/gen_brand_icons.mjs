// 一次性生成器：从 simple-icons / lobehub 拉取常见供应商图标路径，产出 webui/brand-icons.js。
//
// 保留它的唯一理由是记录图标的来源与许可（simple-icons 为 CC0，@lobehub/icons 为 MIT），
// 否则内联进 brand-icons.js 的那堆 path 数据就成了无出处的魔数。产物是源码，运行期
// 绝不联网：WebUI 必须离线可用。改图标集时重跑本脚本，再提交产物。
//
// 用法：node scripts/gen_brand_icons.mjs

import { writeFileSync } from "node:fs";
import path from "node:path";

const SI = (slug) => `https://cdn.jsdelivr.net/npm/simple-icons@latest/icons/${slug}.svg`;
const LOBE = (slug) => `https://cdn.jsdelivr.net/npm/@lobehub/icons-static-svg@latest/icons/${slug}.svg`;

// 预置供应商：只收录「base_url + AMKR 默认路由路径」能拼出正确上游端点的供应商。
//
// 这是刻意的窄口径。AMKR 的默认路由把 `v1/chat/completions` 拼在 base_url 之后，
// 所以 base_url 必须正好是「去掉了 /v1 的根」。以下几家因此被排除——预置一个错的
// 地址比不预置更糟，探测会失败而用户很难看出是地址错了：
//   - Google Gemini：兼容端点在 /v1beta/openai 之下
//   - Perplexity：端点是 /chat/completions，不在 /v1 之下
//   - 智谱 GLM：兼容端点在 /api/paas/v4 之下
//   - NVIDIA NIM / Azure：需要 /v1 之外的厂商私有前缀或 query string 参数
// 这些仍可在对话框里手填名称与地址，再到「高级路径设置」里改路由。
//
// keys 是 id 匹配词：用户常把供应商改名成 `openai-backup`、`my-deepseek` 这类，
// 按子串匹配即可认出品牌。**不能用 brand 去匹配 id**：xAI 的 brand 是单字符 `x`，
// 拿它做子串匹配会把 `my-box` 这类无关 id 也判成 xAI。
const PRESETS = [
  { id: "openai", label: "OpenAI", brand: "openai", baseUrl: "https://api.openai.com", host: "api.openai.com", keys: ["openai"] },
  { id: "anthropic", label: "Anthropic", brand: "anthropic", baseUrl: "https://api.anthropic.com", host: "api.anthropic.com", keys: ["anthropic", "claude"] },
  { id: "deepseek", label: "DeepSeek", brand: "deepseek", baseUrl: "https://api.deepseek.com", host: "api.deepseek.com", keys: ["deepseek"] },
  { id: "moonshot", label: "Moonshot（Kimi）", brand: "moonshot", baseUrl: "https://api.moonshot.cn", host: "api.moonshot.cn", keys: ["moonshot", "kimi"] },
  { id: "qwen", label: "通义千问", brand: "qwen", baseUrl: "https://dashscope.aliyuncs.com/compatible-mode", host: "dashscope.aliyuncs.com", keys: ["qwen", "dashscope", "tongyi"] },
  { id: "xai", label: "xAI（Grok）", brand: "xai", baseUrl: "https://api.x.ai", host: "api.x.ai", keys: ["xai", "grok"] },
  { id: "mistral", label: "Mistral", brand: "mistral", baseUrl: "https://api.mistral.ai", host: "api.mistral.ai", keys: ["mistral"] },
  { id: "groq", label: "Groq", brand: "groq", baseUrl: "https://api.groq.com/openai", host: "api.groq.com", keys: ["groq"] },
  { id: "openrouter", label: "OpenRouter", brand: "openrouter", baseUrl: "https://openrouter.ai/api", host: "openrouter.ai", keys: ["openrouter"] },
  { id: "siliconflow", label: "硅基流动", brand: "siliconcloud", baseUrl: "https://api.siliconflow.cn", host: "api.siliconflow.cn", keys: ["siliconflow", "siliconcloud"] },
  { id: "ollama", label: "Ollama", brand: "ollama", baseUrl: "http://127.0.0.1:11434", host: "127.0.0.1:11434", keys: ["ollama"] },
  { id: "lmstudio", label: "LM Studio", brand: "lmstudio", baseUrl: "http://127.0.0.1:1234", host: "127.0.0.1:1234", keys: ["lmstudio", "lm-studio"] },
  { id: "vllm", label: "vLLM", brand: "vllm", baseUrl: "http://127.0.0.1:8000", host: "127.0.0.1:8000", keys: ["vllm"] },
];

// 品牌 -> 图标来源。只为上面真正用到的品牌取图标，避免把用不上的 path 塞进包里。
const SOURCES = {
  openai: SI("openai"),
  anthropic: SI("anthropic"),
  deepseek: SI("deepseek"),
  moonshot: SI("moonshotai"),
  qwen: SI("qwen"),
  xai: SI("x"),
  mistral: SI("mistralai"),
  groq: LOBE("groq"),
  openrouter: SI("openrouter"),
  siliconcloud: LOBE("siliconcloud"),
  ollama: SI("ollama"),
  lmstudio: SI("lmstudio"),
  vllm: SI("vllm"),
};

const pathOf = (svg) => {
  const match = svg.match(/<path[^>]*\sd="([^"]+)"/);
  if (!match) throw new Error("图标里没有 path d");
  return match[1];
};

const shapes = {};
for (const [brand, url] of Object.entries(SOURCES)) {
  const response = await fetch(url);
  if (!response.ok) throw new Error(`${brand}: HTTP ${response.status}`);
  shapes[brand] = pathOf(await response.text());
  console.log(`OK   ${brand}（${shapes[brand].length} 字符）`);
}

const missing = new Set(PRESETS.map((preset) => preset.brand));
for (const brand of missing) {
  if (!shapes[brand]) throw new Error(`预置用到的品牌缺少图标: ${brand}`);
}

const out = [];
out.push("// 常见供应商的品牌图标与预置信息。");
out.push("//");
out.push("// 图标取自 simple-icons（CC0）与 @lobehub/icons（MIT）的官方矢量，按 24×24 网格");
out.push("// 抽出单条 path 内联于此：WebUI 必须离线可用，不能依赖 CDN、图标字体或网络请求。");
out.push("// 与 icons.js 那套描边图标不同，品牌标志是填充路径，所以另有一套渲染函数。");
out.push("//");
out.push("// 本文件由 scripts/gen_brand_icons.mjs 生成；要改图标集请改那个脚本后重跑。");
out.push("");
out.push('import { svg } from "./dom.js";');
out.push('import { icon } from "./icons.js";');
out.push("");
out.push("// 品牌 -> 单条 path 的 d 属性（viewBox 均为 0 0 24 24）。");
out.push("const BRAND_SHAPES = {");
for (const [brand, d] of Object.entries(shapes)) {
  out.push(`  ${brand}:`);
  out.push(`    "${d}",`);
}
out.push("};");
out.push("");
out.push("// 预置供应商：点一下即可填好名称与地址。");
out.push("//");
out.push("// 只收录「base_url + AMKR 默认路由路径」能拼出正确上游端点的供应商。Gemini、");
out.push("// Perplexity、智谱 的 OpenAI 兼容端点不在 /v1 之下（Perplexity 是 /chat/completions），");
out.push("// 预置它们会拼出错误路径，反而比不预置更难排查，因此刻意留空给用户手填。");
out.push("export const PROVIDER_PRESETS = [");
for (const preset of PRESETS) {
  out.push(`  { id: "${preset.id}", label: "${preset.label}", brand: "${preset.brand}", baseUrl: "${preset.baseUrl}", host: "${preset.host}", keys: [${preset.keys.map((key) => `"${key}"`).join(", ")}] },`);
}
out.push("];");
out.push("");
out.push("// 品牌图标：填充路径，颜色继承 currentColor。未知品牌返回 null，由调用方回退。");
out.push("export function brandIcon(brand, props = {}) {");
out.push("  const d = BRAND_SHAPES[brand];");
out.push("  if (!d) return null;");
out.push('  const { size = 20, class: className, title } = props;');
out.push('  const node = svg("svg", {');
out.push('    viewBox: "0 0 24 24",');
out.push('    fill: "currentColor",');
out.push("    width: size,");
out.push("    height: size,");
out.push('    class: `brand-icon${className ? ` ${className}` : ""}`,');
out.push('    "aria-hidden": title ? null : "true",');
out.push('    role: title ? "img" : null,');
out.push('    "aria-label": title || null,');
out.push("  });");
out.push('  if (title) node.append(svg("title", {}, title));');
out.push('  node.append(svg("path", { d }));');
out.push("  return node;");
out.push("}");
out.push("");
out.push("// 从 base_url 的 host（含非默认端口）或供应商 id 猜品牌，猜不中返回 null。");
out.push("//");
out.push("// 顺序是关键：先按 host 精确匹配（同家供应商常被改名），再退回按 id 子串匹配，");
out.push("// 这样 `openai-backup`、`my-deepseek` 这类自定义命名也能拿到对的图标。");
out.push("export function brandForProvider(id, baseUrl) {");
out.push("  const host = hostOf(baseUrl);");
out.push("  if (host) {");
out.push("    for (const preset of PROVIDER_PRESETS) {");
out.push("      if (host === preset.host || host.endsWith(`.${preset.host}`)) return preset.brand;");
out.push("    }");
out.push("  }");
out.push('  const key = String(id || "").toLowerCase();');
out.push("  if (key) {");
out.push("    for (const preset of PROVIDER_PRESETS) {");
out.push("      if (preset.keys.some((token) => key.includes(token))) return preset.brand;");
out.push("    }");
out.push("  }");
out.push("  return null;");
out.push("}");
out.push("");
out.push("// host 保留非默认端口：本地部署正是靠端口区分 Ollama / LM Studio / vLLM。");
out.push("function hostOf(baseUrl) {");
out.push("  try {");
out.push('    return new URL(String(baseUrl || "")).host.toLowerCase();');
out.push("  } catch {");
out.push('    return "";');
out.push("  }");
out.push("}");
out.push("");
out.push("// 供应商图标：认得出品牌就用品牌标志，否则回退到通用的机架图标。");
out.push("export function providerIcon(id, baseUrl, props = {}) {");
out.push('  return brandIcon(brandForProvider(id, baseUrl), props) || icon("providers", props);');
out.push("}");
out.push("");

const target = path.resolve(import.meta.dirname, "../webui/brand-icons.js");
writeFileSync(target, out.join("\n"), "utf8");
console.log(`\n已写入 ${target}`);
