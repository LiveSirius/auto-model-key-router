# internal/upstream/testdata

本目录存放 Go 侧 `internal/upstream` 所需的**假上游回放语料**，由
`scripts/gen_upstream_fixtures.py` 从真实 Python 实现生成，Go 测试直接消费，
**测试时不需要 Python 解释器**。

## 语料来源

Python 测试（`tests/test_app.py`）里 ~73 处假上游都写成内联闭包：

```python
def handler(request: httpx.Request) -> httpx.Response:
    upstream_payloads.append(json.loads(request.content))
    return httpx.Response(200, json={"id": "ok"})

app.state.runtime_manager.current.http_client = httpx.AsyncClient(
    transport=httpx.MockTransport(handler)
)
```

`handler` 定义在测试函数内部，无法 `import`，仓库里也没有任何 cassette/golden
文件可复用。因此生成器**不静态猜**，而是**真正把 `tests/test_app.py` 跑一遍**，
在 `httpx.MockTransport.handle_async_request` 这一层拦截「代理 → 上游」的
请求/响应，把可观测契约原样落盘。

选这个拦截点是因为它两侧都是真实对象：

- **请求**是代理真正发出的（含 `authorization`、上游路径改写、body 字段顺序、
  超时扩展），不是照着测试源码抄的；
- **响应**是 handler 真正合成的（含状态码、响应头、SSE 分块边界）；
- 参数化测试跑多次，每次成为一条独立记录，天然覆盖失败切换、重试等**调用序列**。

拦截器只读不写：绝不消费响应流（消费会提前触发 SSE、破坏超时类测试），也不改动
请求，因此**不改变任何测试行为**。

## 文件

`python_upstream_fixtures.jsonl` —— JSON Lines，每行一条 request → response 记录，
键按字典序排序（`sort_keys=True`），UTF-8、LF 换行。

## Schema

```jsonc
{
  "id": "0001",              // 语料内唯一序号，等于行号，便于定位失败用例
  "test": "test_provider_target_uses_upstream_model_in_request_body",
                             // 产出该交互的 Python 测试函数名
  "case": "",                // pytest 参数化用例 id，无参数化时为 ""
  "invocation": 0,           // 同一站点内第几次调用（0 起），用于断言调用序列
  "source": {                // 溯源信息，只用于定位，不参与断言
    "file": "tests/test_app.py",
    "test_line": 172,        // 测试函数定义行
    "handler_line": 202,     // handler 函数定义行（运行时归属键）
    "transport_line": 207    // httpx.MockTransport(...) 调用行（= 静态站点）
  },
  "request": {
    "method": "POST",
    "url": "https://upstream.test/v1/chat/completions",
    "path": "/v1/chat/completions",
    "query": "",             // 原始 query，不含 "?"；无 query 时为 ""
    "headers": {             // 小写键；已剔除易变头，见下
      "accept": "*/*",
      "accept-encoding": "identity",
      "authorization": "Bearer sk-main",
      "connection": "keep-alive",
      "content-length": "38",
      "content-type": "application/json",
      "host": "upstream.test"
    },
    "body": { "encoding": "utf-8", "value": "{\"model\":\"vendor-model\",\"messages\":[]}" },
    "timeout": { "connect": 5.0, "pool": 5.0, "read": 5.0, "write": 5.0 }
  },
  "response": {
    "status": 200,
    "headers": { "content-length": "11", "content-type": "application/json" },
    "stream": "bytes",       // bytes | chunks | opaque
    "body_b64": "eyJpZCI6Im9rIn0=",   // 完整响应体（base64），opaque 时为 null
    "chunks_b64": null,      // 仅 stream == "chunks" 时非 null：分块边界
    "opaque_stream": null    // 仅 stream == "opaque" 时非 null：{"class": "..."}
  }
}
```

### 字段说明

- **`request.body.encoding`**：请求体能无损按 UTF-8 解码时为 `"utf-8"`，`value`
  即可读文本（可直接 diff）；否则为 `"base64"`，`value` 是 base64，保证任意字节不丢。
- **`request.timeout`**：httpx 的 per-request 超时扩展。对流式请求，AMKR 会把
  `read` 置为 `null`（不限制读超时），这是 `_stream_upstream` 的对外契约之一，
  因此保留；非流式请求为四个数值。
- **`response.stream`**：
  - `bytes` —— 单一缓冲体，`body_b64` 是全部内容；
  - `chunks` —— 自定义分块流，`chunks_b64` **逐块**记录边界。分块边界是
    SSE 拆分/刷写逻辑的契约（`_stream_upstream`），不能合并；
  - `opaque` —— 流对象没有可读字节属性（`HangingStream` 靠 sleep 表达「挂住」、
    `BrokenStream` 靠抛错表达「解码失败」），无法回放，只记类名。这类站点表达的是
    **超时/异常行为**，Go 侧需自行构造等价物，本语料只提供请求侧契约。
- **`source`** 里的行号只用于人肉溯源；Go 断言应基于 `request`/`response` 本身。

### 有意剔除的字段

- **请求头 `user-agent`**：取值随 `httpx` / `starlette` 版本变化（`python-httpx/x.y.z`
  或 `testclient`），不属于 AMKR 的对外契约。保留它会让语料在升级依赖后无意义地过期。
- **时间戳**：语料不含任何时钟值——源测试用 `monkeypatch` 固定时间或完全不依赖时间。
- **绝对路径**：源测试用 `tmpfile` / `tmp_path`，不会进入上游请求；落盘的路径全部是
  `https://upstream.test` 这类固定测试域名。

## 覆盖率（诚实声明）

生成器每次运行都会打印真实覆盖情况，当前为：

```
[upstream] MockTransport 站点 73 个，取到响应 67 个、交互 103 条
[upstream] 未产出语料的站点 6 个（原因见括号）:
  - tests/test_app.py::test_anthropic_count_tokens_is_served_locally:600 (handler 未被调用（请求在到达上游前已返回）)
  - tests/test_app.py::test_first_byte_timeout_with_single_key_does_not_retry_same_key:1480 (handler 被超时取消，未产生响应)
  - tests/test_app.py::test_first_byte_timeout_with_requested_key_does_not_retry_same_key:1517 (handler 被超时取消，未产生响应)
  - tests/test_app.py::test_task_name_rejects_caller_supplied_sampling_params:2974 (handler 未被调用（请求在到达上游前已返回）)
  - tests/test_app.py::test_task_name_rejects_caller_supplied_key:3037 (handler 未被调用（请求在到达上游前已返回）)
  - tests/test_app.py::test_task_stop_conflicts_with_anthropic_stop_sequences:3316 (handler 未被调用（请求在到达上游前已返回）)
```

两类缺口都**不是提取失败**，而是这些交互在上游根本没有产生响应：

1. **handler 未被调用**（4 个）：测试断言的是**本地拒绝**路径——`count_tokens`
   在本地计算、task 参数校验在到达上游前就报错。没有上游交互可记录，属预期。
2. **handler 被超时取消**（2 个）：测试断言首字节超时后**不重试**，handler 因
   `anyio.sleep` 被取消，从未合成响应。请求本身已可从上文的超时类站点推断，
   但严格来说这两条的请求侧契约**未被本语料覆盖**。

Go 侧只能断言 103 条真实交互；上述 6 个站点需另行用 Go 原生测试覆盖。

## 重新生成

```powershell
cd D:\Code\auto-model-key-router
$env:PYTHONIOENCODING='utf-8'
python -X utf8 scripts/gen_upstream_fixtures.py           # 写入语料，退出码 0
python -X utf8 scripts/gen_upstream_fixtures.py --check   # 只校验，不写盘
```

`--check` 是 CI 新鲜度门禁：语料最新时退出 0，缺失或过期时退出 1 并把原因写到
stderr。它**必须先跑一遍测试**才能重放交互，因此耗时与
`pytest tests/test_app.py` 相当（约 2 分钟），比其它纯函数语料生成器慢——这是
「语料必须由真实实现产出」的直接代价。采集时若 `tests/test_app.py` 不是全绿
（`pytest` 退出码非 0），生成器会以退出码 2 失败，宁可不出语料也不用失败运行的
产物。

新增 `httpx.MockTransport` 站点后重新生成即可，站点会被自动发现（AST 扫描
`tests/test_app.py`）。
