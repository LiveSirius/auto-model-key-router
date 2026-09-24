// Package configops 实现配置字典的纯操作：增删改查 provider / model / target /
// 任务 / unified_model，以及随之而来的引用修复。
//
// 移植 auto_model_key_router/config_operations.py（1282 行）。这一层不碰 I/O、
// 网络与数据库，输入输出都是 internal/canonical 的 JSON 值，因此可以逐条测试。
//
// # 数据模型
//
// data 是一个 *canonical.Value（JSON 对象），所有函数**就地**修改它；这与
// Python 侧「传 dict 进来、改完不返回」的契约一致——调用方随后把同一份 data
// 交给 SaveConfigData 落盘。返回 *canonical.Value 的函数（create_model 等）
// 返回的是**已经插入 data 里的那个对象**，不是副本，改它会同时改 data。
//
// # 与 Python 的映射
//
//   - ConfigOperationError ↔ ConfigOperationError(message, status_code)。它是
//     ValueError 的子类，但带 status_code，管理 API 直接把它当 HTTP 状态码。
//   - Python 的 `str | None` 参数映射为 *string，nil 表示 None。这个区分不是
//     形式主义：update_model(new_id=None) 表示「不改」，而 new_id="" 会走
//     _non_empty 报 422。
//   - Python 默认值为 True 的布尔参数（如 enabled）映射为 *bool，nil 表示
//     使用 Python 默认值；默认值为 False 的用 bool。
//   - 真值判断一律走 canonical 的 Truthy / StringValue，不要用 != "" 或 != 0：
//     Python 的 `or` 把 0、空串、空列表、false、空对象都当假。
//
// # 刻意保留的参照实现行为（看起来像 bug，但刻意保留）
//
//   - Providers/Models/ProviderKeys/ModelTargets 会 setdefault：读一次就把
//     空容器写进 data（config_operations.py:26）。连 fallback_model_id 这种
//     「只读」函数都会通过 ModelTargets 往每个模型里塞 targets: []。
//   - 重命名 provider/key/model 用 `d[k] = d.pop(old)`，因此被改名的条目会
//     移到对象的**末尾**（config_operations.py:133）。
//   - update_provider 在 data 里搬 upstream_routes 时只搬旧的顶层键，且
//     provider["base_url"] 为空时会抛裸 ValueError（不是 ConfigOperationError）。
//   - set_key_service_models 的 desired 是 Python set，因此**一次新增多个模型**
//     时写入 models 的顺序取决于 Python 的字符串哈希（进程间随机）。Go 侧按调用方
//     给出的顺序依次创建；详见该函数与 TestSetKeyServiceModelsCreatesModelsInCallerOrder。
//
// # 已知未复刻的边界
//
//   - normalize_base_url 里的 URL 校验复刻 urllib.parse.urlsplit 的 scheme/netloc
//     提取（含 C0 控制字符剥离与 IPv6 括号校验），但不复刻 netloc 内部更细的
//     校验；`https://exa mple.com`、`http://host:bad` 这类 Python 接受的输入
//     Go 侧同样接受，行为一致。
//   - str.strip() 与 strings.TrimSpace 对 U+001C..U+001F 这四个分隔符的处理不同
//     （Python 视为空白，Go 不视为空白）。这类输入不是合法配置。
//   - Go 侧参数按 Python 类型注解映射（见上），把 base_url 传成数字这类契约外
//     调用不在用例覆盖范围内；反之，**data 内部的任意 JSON 值**都完整覆盖。
package configops
