// Package config 解析、迁移与持久化 router-config.json。
//
// 兼容性目标是「字节级可用」：现存的 config_version 4 配置文件必须能被 Go 直接
// 读取并写回，且写回后的内容与 Python 版本一致。因此本包刻意保留若干看起来
// 可以简化的行为：
//
//   - 字段顺序按插入顺序落盘（不排序），避免每次保存都重排整份文件；
//   - 整数与浮点分别渲染（60 与 60.0），与 canonical 包一致；
//   - 迁移逻辑保留 v1/v2 的列表布局与 v3 的 pool 白名单语义，包括其中已知的
//     怪异之处，因为已存在的配置文件依赖这些行为。
package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"

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

func isFloatLiteral(literal string) bool {
	return strings.ContainsAny(literal, ".eE") || literal == "NaN" ||
		strings.Contains(literal, "Infinity")
}

// configVersionOf 复刻 Python 的 “int(normalized.get("config_version") or 0)“。
//
// 三种结果都要区分，因为 Python 对它们的行为不同：
//   - 成功：字符串/数字/布尔都能转（int("4")=4、int(4.7)=4、int(True)=1）；
//   - ValueError：字符串不是合法整数（"four"）；
//   - TypeError：列表、字典等不可转换。
//
// 失败时回传 Python 的原始异常文本。调用方据此决定是当作契约错误上报
// （ValueError），还是当作内部错误（TypeError，语料中记录了原始文本）。
func configVersionOf(raw *canonical.Value) (int, error) {
	value := raw.Lookup("config_version")
	if value == nil || !value.Truthy() {
		return 0, nil
	}
	switch value.Kind {
	case canonical.KindNumber:
		if isFloatLiteral(value.Num) {
			f, ok := value.AsFloat()
			if !ok || math.IsNaN(f) {
				return 0, errInternal("cannot convert float NaN to integer")
			}
			if math.IsInf(f, 0) {
				return 0, errInternal("cannot convert float infinity to integer")
			}
			return int(f), nil
		}
		n, ok := value.AsInt()
		if !ok {
			// 超出 int64：Python 任意精度可表示，但配置版本不会是这种值。
			return 0, errInternal("int() argument is out of range")
		}
		return int(n), nil
	case canonical.KindBool:
		if value.Bool {
			return 1, nil
		}
		return 0, nil
	case canonical.KindString:
		text := strings.TrimSpace(value.Str)
		n, err := strconv.Atoi(text)
		if err != nil {
			return 0, &ConfigError{Message: fmt.Sprintf(
				"invalid literal for int() with base 10: '%s'", value.Str)}
		}
		return n, nil
	}
	return 0, errInternal(
		"int() argument must be a string, a bytes-like object or a real number, not '%s'",
		pyTypeName(value))
}
