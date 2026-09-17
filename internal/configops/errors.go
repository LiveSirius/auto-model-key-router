package configops

import (
	"fmt"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
)

// ConfigOperationError 是一次配置操作的可展示失败。
//
// 移植 config_operations.py:17。Python 侧它是 ValueError 的子类，并额外带一个
// status_code 属性（默认 400）；管理 API 直接把它当 HTTP 状态码使用。Go 侧因此
// 把它建模成带 StatusCode 字段的 error 类型，调用方用 errors.As 取回：
//
//	var opErr *configops.ConfigOperationError
//	if errors.As(err, &opErr) { w.WriteHeader(opErr.StatusCode) }
//
// 错误文本（Message）是对外契约，与 Python 逐字一致，由 testdata 语料断言。
type ConfigOperationError struct {
	Message    string
	StatusCode int
}

func (e *ConfigOperationError) Error() string { return e.Message }

// opErr 构造一个 ConfigOperationError。
func opErr(statusCode int, message string) *ConfigOperationError {
	return &ConfigOperationError{Message: message, StatusCode: statusCode}
}

// opErrf 构造一个带格式化文本的 ConfigOperationError。
func opErrf(statusCode int, format string, args ...any) *ConfigOperationError {
	return &ConfigOperationError{Message: fmt.Sprintf(format, args...), StatusCode: statusCode}
}

// PyError 复刻 Python 内建异常的**类型名**与消息文本。
//
// 参照实现里并非所有失败都是 ConfigOperationError：migrate_config_data 的版本
// 过高、normalize_upstream_base_url 的空 URL、`for x in None`、`int("abc")` 等
// 都会作为裸 ValueError / TypeError / AttributeError 冒到调用方。这些分支对外
// 表现为 500 而不是 400，属于可观察行为，所以 Go 侧不能一律折叠成
// ConfigOperationError——对拍语料会断言「是不是 ConfigOperationError」。
//
// TypeName 用 Python 的类名（ValueError / TypeError / AttributeError），便于
// 语料比对时把 Go 错误分类映射回 Python 异常类型。
type PyError struct {
	TypeName string
	Message  string
}

func (e *PyError) Error() string { return e.Message }

// pyAttributeError 复刻 `obj.attr` 取属性失败。
//
// config_operations.py 里 data 一律被当作 dict 使用（data.setdefault、
// data.get、data.pop），传入非 dict 时 Python 抛 AttributeError。
func pyAttributeError(value *canonical.Value, attribute string) *PyError {
	return &PyError{
		TypeName: "AttributeError",
		Message:  fmt.Sprintf("'%s' object has no attribute '%s'", canonical.PyTypeName(value), attribute),
	}
}
