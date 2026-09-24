// Package logfiles 移植 auto_model_key_router/log_files.py：日志文件轮转。
//
// 三个函数都直接落在文件系统上，因此夹具必须钉在临时目录里、只比较**相对**
// 路径，测试建自己的临时目录覆盖同一批操作。
//
// 与 Python 的两处已记录差异（都有具名测试锁定）：
//
//   - 通配符用 Go 的 filepath.Match，而 pathlib 用 fnmatch：字符类的取反写法
//     不同（Python 是 `[!b]`，Go 是 `[^b]`），Go 把 `\` 当转义而 Python 不认。
//     日志文件名里出现这些字符不现实，用例只用两边一致的形态。
//   - pathlib 在 Windows 上 glob 不区分大小写，Go 侧一律区分。
//
// Python 的 “Path“ 会把路径归一化（`//`、`.`、结尾分隔符）后再拼归档名；
// Go 侧直接用 filepath，其清理规则在常规输入上与之等价（且同样把路径交给
// 操作系统的分隔符规则），但刻意不复刻 PureWindowsPath 的盘符特例。
package logfiles

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ErrEmptyName 对应 Python 在路径没有名字时抛出的 ValueError（log_files.py:23）。
//
// 触发条件是输入为空、"."、"/" 这类「剥掉结尾分隔符与 . 分量后什么都不剩」的路径；
// Python 的 PurePath.with_name 在这种情况下抛 ValueError，Go 侧返回本错误。
var ErrEmptyName = errors.New("路径没有文件名")

// archiveTimestampLayout 对应 Python 的 strftime("%Y%m%d-%H%M%S")。
const archiveTimestampLayout = "20060102-150405"

// fallbackSuffix 是 Python 里 `path.suffix or ".log"` 的回退后缀：
// 没有后缀（含点开头的隐藏名、结尾是点、无扩展名）时，归档文件补 .log。
const fallbackSuffix = ".log"

// ArchiveCurrentLog 归档当前日志文件，并保持原路径可用（log_files.py:7）。
//
// 语义与 Python 完全一致：
//
//   - 父目录先按需创建（含多级）；
//   - 文件存在且**长度>0** 时改成归档名，并在原路径新建空文件，返回归档路径；
//   - 其余情况（不存在、长度为 0）只把原路径截断/建空，返回的归档路径为空串。
//
// now 在 Python 里是函数内部的 datetime.now()，这里提成显式参数便于测试
// （用例把时刻钉在 2026-01-02T03:04:05）；真实调用方传 time.Now()。
func ArchiveCurrentLog(logFilePath string, now time.Time) (archivePath string, archived bool, err error) {
	parent := pyParent(logFilePath)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", false, err
	}
	info, statErr := os.Stat(logFilePath)
	switch {
	case statErr == nil && info.Size() > 0:
		target, err := NextLogArchivePath(logFilePath, now)
		if err != nil {
			return "", false, err
		}
		if err := os.Rename(logFilePath, target); err != nil {
			return "", false, err
		}
		if err := touch(logFilePath); err != nil {
			return "", false, err
		}
		return target, true, nil
	case statErr != nil && !errors.Is(statErr, fs.ErrNotExist):
		// Python 的 path.exists() 只把「不存在」当假，权限等其他错误会由后面的
		// write_text 抛出；这里同样直接上报。
		return "", false, statErr
	default:
		if err := os.WriteFile(logFilePath, nil, 0o666); err != nil {
			return "", false, err
		}
		return "", false, nil
	}
}

// touch 等价于 Path.touch()：不存在则新建空文件，存在则只刷新时间戳。
func touch(path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return err
	}
	return file.Close()
}

// NextLogArchivePath 返回不与现有文件冲突的下一个归档路径（log_files.py:19）。
//
// 归档名是 `stem.时间戳.后缀`；后缀为空时回退成 .log（所以无扩展名的 server
// 会归档成 server.<ts>.log）。同名已存在时依次尝试 `.1`、`.2`……。
//
// 注意 Python 的 PurePath.suffix 与 filepath.Ext 不同：`server.`、`.hidden`
// 在 Python 里都没有后缀（末位点不算、点开头不算），这里按 Python 的规则实现。
func NextLogArchivePath(logFilePath string, now time.Time) (string, error) {
	name, err := pyPathName(logFilePath)
	if err != nil {
		return "", err
	}
	stem, suffix := pyStemSuffix(name)
	if suffix == "" {
		suffix = fallbackSuffix
	}
	base := fmt.Sprintf("%s.%s", stem, now.Format(archiveTimestampLayout))
	candidate := pyWithName(logFilePath, base+suffix)
	for index := 1; pathExists(candidate); index++ {
		candidate = pyWithName(logFilePath, fmt.Sprintf("%s.%d%s", base, index, suffix))
	}
	return candidate, nil
}

// ArchivedLogPaths 列出已有的归档文件，按文件名倒序（log_files.py:31）。
//
// 匹配规则是 `stem.*后缀`（后缀同样有 .log 回退），过滤掉目录与非普通文件，
// 并排除日志文件自身。父目录不存在或不可读时返回空列表——Python 的 pathlib
// 会吞掉 OSError，这里同样不上报错误。
//
// 通配符语义见包注释：与 fnmatch 在字符类取反/转义上有差异，用例只覆盖
// 两边一致的部分。
func ArchivedLogPaths(logFilePath string) []string {
	name := pyPathNameIgnoringError(logFilePath)
	stem, suffix := pyStemSuffix(name)
	if suffix == "" {
		suffix = fallbackSuffix
	}
	pattern := stem + ".*" + suffix
	parent := pyParent(logFilePath)
	entries, err := os.ReadDir(parent)
	if err != nil {
		return nil
	}
	self := filepath.Clean(logFilePath)
	var names []string
	for _, entry := range entries {
		matched, matchErr := filepath.Match(pattern, entry.Name())
		if matchErr != nil || !matched {
			continue
		}
		full := filepath.Join(parent, entry.Name())
		// Python 用 `candidate != path` 排除日志文件自身：两边都按归一化后的
		// 字符串比较（Windows 上 PurePath 还会忽略大小写，见包注释）。
		if full == self {
			continue
		}
		info, statErr := os.Stat(full)
		if statErr != nil || !info.Mode().IsRegular() {
			continue
		}
		names = append(names, entry.Name())
	}
	// 按文件名倒序：调用方按这个顺序展示「最近的归档」。
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	paths := make([]string, 0, len(names))
	for _, item := range names {
		paths = append(paths, filepath.Join(parent, item))
	}
	return paths
}

// pathExists 等价于 Path.exists()：能 stat 到即为真（目录也算）。
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// pyPathName 返回 Python PurePath.name（log_files.py 里所有 with_name 的前提）。
func pyPathName(path string) (string, error) {
	name := pyPathNameIgnoringError(path)
	if name == "" {
		return "", fmt.Errorf("%w: %q", ErrEmptyName, path)
	}
	return name, nil
}

// pyPathNameIgnoringError 取路径最后一段，空名字返回空串。
//
// 规则来自 PurePath._parse_path：先丢掉结尾分隔符，再丢掉空分量与 "." 分量，
// 保留 ".."；Windows 上盘符（C:）本身不算名字。
func pyPathNameIgnoringError(path string) string {
	name := ""
	for _, part := range strings.FieldsFunc(path, isPathSeparator) {
		switch {
		case part == "" || part == ".":
			// 空分量与 "." 都被 PurePath 丢弃。
		case isWindowsDrive(part):
			name = ""
		default:
			name = part
		}
	}
	return name
}

// isWindowsDrive 判断分量是否是纯盘符（"C:"），它在 WindowsPath 里不算名字。
//
// 只在 Windows 上生效：Linux 下 "C:" 是完全合法的文件名，PurePosixPath("a/C:").name
// 就是 "C:"。
func isWindowsDrive(part string) bool {
	if filepath.Separator != '\\' {
		return false
	}
	if len(part) != 2 || part[1] != ':' {
		return false
	}
	char := part[0]
	return (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z')
}

// isPathSeparator 与 Python 的平台规则一致：Windows 同时认 "/" 与 "\"，
// 其他平台只认 "/"。
func isPathSeparator(char rune) bool {
	if char == '/' {
		return true
	}
	return filepath.Separator == '\\' && char == '\\'
}

// pyStemSuffix 复刻 PurePath.stem / PurePath.suffix（pathlib.py 的 rfind('.') 规则）：
// 最后一个点必须在名字中间才算后缀，因此 "server."（末位点）与 ".hidden"（点开头）
// 都没有后缀。这与 filepath.Ext 不同，必须单独实现。
func pyStemSuffix(name string) (stem, suffix string) {
	index := strings.LastIndexByte(name, '.')
	if index > 0 && index < len(name)-1 {
		return name[:index], name[index:]
	}
	return name, ""
}

// pyParent 返回 Python PurePath.parent 的字符串形式。
//
// 空路径的 parent 是 "."，而根路径（"/"、"///"）的 parent 仍是根——Python 在
// PurePath.__init__ 里就把多余的 "." 与空分量清掉了，这里用同样的方式判断。
func pyParent(path string) string {
	trimmed := strings.TrimRightFunc(path, isPathSeparator)
	if trimmed == "" {
		// 输入是 "/"（Windows 上还有 "\"）或它的重复：parent 仍是根。
		if isRooted(path) {
			return string(filepath.Separator)
		}
		return "."
	}
	return filepath.Dir(trimmed)
}

// isRooted 判断路径是否以分隔符开头。
func isRooted(path string) bool {
	if strings.HasPrefix(path, "/") {
		return true
	}
	return filepath.Separator == '\\' && strings.HasPrefix(path, "\\")
}

// pyWithName 等价于 PurePath.with_name：父目录 + 新名字。
//
// filepath.Join 会顺手清理路径，与 PurePath 把 "." 分量丢掉的行为一致；
// 父目录为 "." 时 Join 不会留下 "./" 前缀，也与 PurePath 一致。
func pyWithName(path, name string) string {
	return filepath.Join(pyParent(path), name)
}
