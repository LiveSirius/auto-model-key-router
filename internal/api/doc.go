// Package api 实现 AMKR 的管理 API，是 auto_model_key_router/management_api.py 的
// Go 移植（47 条路由）。
//
// 三条贯穿全包的设计约束：
//
//  1. **响应体必须与 Python 逐字节一致**。所有 JSON 都经 internal/canonical 序列化
//     （保留键插入顺序、不转义非 ASCII、整数与浮点分别渲染），绝不用 encoding/json：
//     后者会按字典序重排键、把 60.0 渲染成 60、把中文转成 \uXXXX。
//     唯一的例外是任务的 `display_name`（Go 侧新增，Python 没有）：它**只在任务真的
//     取了名时**才写进响应，因此没取名的任务响应仍逐字节不变，47 条路由语料里只有
//     tasks/create-missing-all 因 `model` 移出必填而手工改动（见 CHANGELOG）。
//  2. **HTTP 层只用标准库**。Go 1.22+ 的 ServeMux 原生支持
//     `"GET /api/models/{model_id}"` 形式的「方法 + 模板」路由，因此不需要任何
//     Web 框架；路径参数用 r.PathValue 取。
//  3. **刻意不做「合理化」**。参照实现里有若干看起来是 bug 的行为（同一句错误在
//     不同调用深度分别表现为 400 与 500、路径参数只匹配 id 不匹配别名、DELETE 的
//     请求体可选与必填并存），都已逐条保留并在测试里命名锁定，见各文件注释。
package api
