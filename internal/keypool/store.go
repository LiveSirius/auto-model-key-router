package keypool

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
)

// CapabilityStore 持久化原生端点探测结果。
type CapabilityStore struct {
	path string
}

// NewCapabilityStore 构造持久化存储。
func NewCapabilityStore(path string) *CapabilityStore {
	return &CapabilityStore{path: path}
}

// Path 返回存储文件路径。
func (s *CapabilityStore) Path() string { return s.path }

// Load 读取探测状态。
//
// 对齐 endpoint_capability_store.py:15：文件缺失、无法解析、或结构不对时一律返回
// 空对象——缓存损坏不该阻止启动。同时兼容两种键名（`endpoint_capabilities` 与
// 旧版的 `url_native_support`）。
func (s *CapabilityStore) Load() *canonical.Value {
	content, err := os.ReadFile(s.path)
	if err != nil {
		return canonical.NewObject()
	}
	raw, err := canonical.ParseString(string(content))
	if err != nil || !raw.IsObject() {
		return canonical.NewObject()
	}
	for _, field := range []string{"endpoint_capabilities", "url_native_support"} {
		candidate := raw.Lookup(field)
		if candidate.IsObject() {
			return candidate
		}
	}
	return canonical.NewObject()
}

// Save 原子写入探测状态。
//
// 载荷结构固定为 {"version": 1, "endpoint_capabilities": {已排序}}，与参照实现
// 一致。写入失败只返回错误、不 panic；调用方（UpdateNativeEndpoint）可以选择
// 忽略，因为这是缓存而非数据源。
func (s *CapabilityStore) Save(states *canonical.Value) error {
	payload := canonical.NewObjectOf(
		canonical.ObjectPair{Key: "version", Value: canonical.NewIntValue(1)},
		canonical.ObjectPair{Key: "endpoint_capabilities", Value: sortedObject(states)},
	)
	suffix, err := randomHex(6)
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	temporaryPath := filepath.Join(dir,
		fmt.Sprintf(".%s.%s.tmp", filepath.Base(s.path), suffix))
	defer func() { _ = os.Remove(temporaryPath) }()

	content := canonical.DumpsIndent(payload, 2) + "\n"
	if err := os.WriteFile(temporaryPath, []byte(translateNewlines(content)), 0o644); err != nil {
		return err
	}
	return os.Rename(temporaryPath, s.path)
}

// sortedObject 按键排序重建对象。
//
// 参照实现用 dict(sorted(states.items()))，因此落盘时键有序。Go 的 map 迭代顺序
// 随机，必须显式排序，否则同一份状态每次写出不同字节。
func sortedObject(value *canonical.Value) *canonical.Value {
	result := canonical.NewObject()
	if !value.IsObject() {
		return result
	}
	keys := value.Obj.Keys()
	slices.Sort(keys)
	for _, key := range keys {
		result.SetKey(key, value.Lookup(key))
	}
	return result
}

// translateNewlines 复刻 Python 文本模式的换行翻译（Windows 上是 CRLF）。
//
// 与 config 包同样的处理：Python 的 write_text 会把 "\n" 翻译成 os.linesep，
// Go 的 os.WriteFile 不翻译。这里不引入对 config 包的依赖，保持 keypool 只依赖
// canonical——两处实现都很小，重复优于跨层耦合。
func translateNewlines(content string) string {
	if runtime.GOOS != "windows" {
		return content
	}
	return strings.ReplaceAll(content, "\n", "\r\n")
}

// randomHex 返回 n 字节的十六进制文本。
func randomHex(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
