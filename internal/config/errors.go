package config

import (
	"errors"
	"fmt"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
)

// ConfigError 表示配置内容不合法。
//
// 对应 Python 侧抛出的 ValueError。错误文本会经 management API 原样回给用户，
// 属于对外契约，故与 Python 的措辞保持一致（由 testdata 语料逐条断言）。
type ConfigError struct {
	Message string
}

func (e *ConfigError) Error() string { return e.Message }

// InternalError 表示输入触发了参照实现的意外异常（TypeError、AttributeError 等）。
//
// 这类输入不是合法配置，正常路径不会产生。Go 侧同样必须失败关闭（不能静默产出
// 与 Python 不同的配置），但不复刻 Python 的异常文本——那是解释器的实现细节。
type InternalError struct {
	Message string
}

func (e *InternalError) Error() string { return e.Message }

// errf 构造 ConfigError（对外契约错误）。
func errf(format string, args ...any) error {
	return &ConfigError{Message: fmt.Sprintf(format, args...)}
}

// errInternal 构造 InternalError。
func errInternal(format string, args ...any) error {
	return &InternalError{Message: fmt.Sprintf(format, args...)}
}

// pyTypeName 返回 Python 侧的类型名，用于复刻 TypeError 文本。
func pyTypeName(v *canonical.Value) string { return canonical.PyTypeName(v) }

// configVersionOf 复刻 Python 的 “int(normalized.get("config_version") or 0)“。
//
// 三种结果都要区分，因为 Python 对它们的行为不同：
//   - 成功：字符串/数字/布尔都能转（int("4")=4、int(4.7)=4、int(True)=1）；
//   - ValueError：字符串不是合法整数（"four"）——契约错误，文本原样上报；
//   - TypeError / OverflowError：列表、字典等不可转换——内部错误。
//
// 注意配置版本可能是 4.0、4.7、true、null 或缺失；只有 v3 迁移会把它写成整数 4。
func configVersionOf(raw *canonical.Value) (int, error) {
	value := raw.Lookup("config_version")
	if value == nil || !value.Truthy() {
		return 0, nil
	}
	parsed, err := canonical.ToInt(value)
	if err != nil {
		var valueErr *canonical.ValueError
		if errors.As(err, &valueErr) {
			return 0, &ConfigError{Message: valueErr.Message}
		}
		return 0, errInternal("%s", err.Error())
	}
	return int(parsed), nil
}
