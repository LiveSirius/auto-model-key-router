package logfiles

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 本文件是**参照实现的 uvicorn_log_config 的 Go 侧落点**：Python 用 logging 的
// dictConfig 把日志交给 uvicorn 的 FileHandler（service.py 的 uvicorn_log_config），
// Go 没有 logging，因此这里用 slog.Handler 直接实现同一个落点与同一套行格式。
//
// 行格式对齐参照实现的 formatter
// `%(asctime)s %(levelname)s %(name)s %(message)s`：
//
//	2026-09-18 11:32:41,533 WARNING auto_model_key_router.app upstream stream error {...}
//
// 与 Python 的四处已记录差异：
//
//   - **没有 status_code 等级提升**。参照实现给 file handler 挂了
//     AccessLogLevelFilter，但它读 `record.status_code`，而 uvicorn 的访问日志用
//     `logger.info('%s - "%s %s HTTP/%s" %d', ...)` 的位置参数记录，
//     `getattr(record, "status_code", None)` 恒为 None，过滤器从不生效。历史日志
//     实测：1,618,926 行 uvicorn.access **全部 INFO**，WARNING/ERROR 的
//     uvicorn.access 为 0 行（503/404 也不例外）。这里照实保留「一律 INFO」。
//   - **属性渲染成 JSON 对象追加在消息之后**。参照实现是调用方自己
//     `json.dumps(payload, ensure_ascii=False)` 拼进消息；Go 侧由 handler 统一渲染，
//     因此行的可读性一致，但键顺序按调用方传参顺序，不保证与 Python 逐字相同。
//   - **WithGroup 被忽略**（与仓库内其它 handler 一致）；本仓库没有 WithGroup
//     调用点。
//   - **日志只落文件，不再同时写 stderr**。与参照实现相同：uvicorn 拿到
//     log_config 后其 logger 只剩 file handler，控制台不再出现这些行。

// pythonTimeLayout 是 Python logging 默认的 asctime：`%Y-%m-%d %H:%M:%S,mmm`。
const pythonTimeLayout = "2006-01-02 15:04:05,000"

// AppLoggerName 对应 `logging.getLogger("auto_model_key_router.app")`
// （proxy_handler.py 的 LOGGER），也是本服务全部应用侧日志的 logger 名。
const AppLoggerName = "auto_model_key_router.app"

// AccessLoggerName 对应 uvicorn 的访问日志 logger。
const AccessLoggerName = "uvicorn.access"

// ServerLoggerName 对应 uvicorn 的服务器日志 logger。参照实现里启动/关停与
// WebSocket 握手结果都记在这里（uvicorn 的 `self.logger`），实测历史日志形如
// `INFO uvicorn.error Started server process [4732]`、`INFO uvicorn.error 127.0.0.1:54468
// - "WebSocket /ws/events" 403`。
const ServerLoggerName = "uvicorn.error"

// Sink 是写到同一个日志文件的一组 logger，对应 uvicorn_log_config 的 file handler
// 在 Go 侧的等价物。
//
// 三个 logger 共用同一个文件与同一把写锁：参照实现里所有 logger 的 handler 都指向
// 同一个 FileHandler，行与行之间不会交错。
type Sink struct {
	// App 用于应用侧结构化日志（交给 internal/proxy、internal/wsproxy 等），
	// 对应 `logging.getLogger("auto_model_key_router.app")`。
	App *slog.Logger
	// Access 用于 HTTP 访问日志（见 internal/server 的 accessLog）。
	Access *slog.Logger
	// Server 用于服务器自身的生命周期与握手日志（启动、关停、升级被拒）。
	Server *slog.Logger

	file *os.File
}

// Open 打开（按需创建）日志文件并返回 Sink。
//
// 对应 uvicorn_log_config 的两处副作用：父目录按需创建
// （`path.parent.mkdir(parents=True, exist_ok=True)`），以及以**追加**方式打开文件
// （logging.FileHandler 的默认 mode="a"）。追加而非截断不可省：前台启动前已经归档过
// 旧日志（ArchiveLogForForeground），截断会把归档后新写入的内容抹掉。
func Open(path string) (*Sink, error) {
	if err := os.MkdirAll(pyParent(path), 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o666)
	if err != nil {
		return nil, err
	}
	mu := &sync.Mutex{}
	return &Sink{
		App:    slog.New(&fileHandler{name: AppLoggerName, file: file, mu: mu}),
		Access: slog.New(&fileHandler{name: AccessLoggerName, file: file, mu: mu}),
		Server: slog.New(&fileHandler{name: ServerLoggerName, file: file, mu: mu}),
		file:   file,
	}, nil
}

// Close 关闭底层文件。
func (s *Sink) Close() error {
	if s == nil || s.file == nil {
		return nil
	}
	return s.file.Close()
}

// fileHandler 把 slog 记录按 Python logging 的行格式写进一个文件。
type fileHandler struct {
	name  string
	file  io.Writer
	mu    *sync.Mutex
	attrs []slog.Attr
}

// Enabled 对应 file logger 的 level="INFO"。
func (h *fileHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo
}

// Handle 写出一行。写入串行化：slog 允许并发调用 Handle，一行应当原子落盘。
func (h *fileHandler) Handle(_ context.Context, record slog.Record) error {
	var line strings.Builder
	line.WriteString(record.Time.Format(pythonTimeLayout))
	line.WriteByte(' ')
	line.WriteString(pythonLevelName(record.Level))
	line.WriteByte(' ')
	line.WriteString(h.name)
	line.WriteByte(' ')
	line.WriteString(record.Message)
	if attrs := formatAttrs(h.attrs, &record); attrs != "" {
		line.WriteByte(' ')
		line.WriteString(attrs)
	}
	line.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.file, line.String())
	return err
}

// WithAttrs 累积属性。mu 是指针，因此这里的结构体拷贝不会复制锁。
func (h *fileHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	clone := *h
	clone.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &clone
}

// WithGroup 被忽略，见文件头说明。
func (h *fileHandler) WithGroup(string) slog.Handler { return h }

// pythonLevelName 复刻 Python 的 levelname（WARN 在 Python 里叫 WARNING）。
func pythonLevelName(level slog.Level) string {
	switch {
	case level < slog.LevelInfo:
		return "DEBUG"
	case level < slog.LevelWarn:
		return "INFO"
	case level < slog.LevelError:
		return "WARNING"
	default:
		return "ERROR"
	}
}

// formatAttrs 把属性渲染成 json.dumps 风格的 JSON 对象；没有属性时返回空串。
//
// 分隔符用 json.dumps 的默认值（", " 与 ": "），非 ASCII 不转义
// （对应 `ensure_ascii=False`）。
func formatAttrs(extra []slog.Attr, record *slog.Record) string {
	if len(extra)+record.NumAttrs() == 0 {
		return ""
	}
	var out strings.Builder
	out.WriteByte('{')
	index := 0
	writeAttr := func(attr slog.Attr) {
		if index > 0 {
			out.WriteString(", ")
		}
		index++
		out.WriteString(strconv.Quote(attr.Key))
		out.WriteString(": ")
		writeJSONValue(&out, attr.Value)
	}
	for _, attr := range extra {
		writeAttr(attr)
	}
	record.Attrs(func(attr slog.Attr) bool {
		writeAttr(attr)
		return true
	})
	out.WriteByte('}')
	return out.String()
}

// writeJSONValue 按 json.dumps 的取值规则渲染单个属性值。
func writeJSONValue(out *strings.Builder, value slog.Value) {
	switch value.Kind() {
	case slog.KindString:
		out.WriteString(strconv.Quote(value.String()))
	case slog.KindInt64:
		out.WriteString(strconv.FormatInt(value.Int64(), 10))
	case slog.KindUint64:
		out.WriteString(strconv.FormatUint(value.Uint64(), 10))
	case slog.KindFloat64:
		out.WriteString(strconv.FormatFloat(value.Float64(), 'g', -1, 64))
	case slog.KindBool:
		out.WriteString(strconv.FormatBool(value.Bool()))
	case slog.KindDuration:
		out.WriteString(strconv.Quote(value.Duration().String()))
	case slog.KindTime:
		out.WriteString(strconv.Quote(value.Time().Format(time.RFC3339Nano)))
	default:
		writeJSONAny(out, value.Any())
	}
}

// writeJSONAny 渲染 KindAny。参照实现的 payload 是 dict[str, Any]，数值与布尔要落成
// JSON 字面量（`"status_code": 200`），不能一律加引号。
func writeJSONAny(out *strings.Builder, value any) {
	switch typed := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		out.WriteString(strconv.FormatBool(typed))
	case int:
		out.WriteString(strconv.Itoa(typed))
	case int64:
		out.WriteString(strconv.FormatInt(typed, 10))
	case float64:
		out.WriteString(strconv.FormatFloat(typed, 'g', -1, 64))
	case error:
		out.WriteString(strconv.Quote(typed.Error()))
	default:
		out.WriteString(strconv.Quote(fmt.Sprint(typed)))
	}
}
