package logfiles

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fixedNow 是具名测试统一使用的注入时刻（与语料同一时刻）。
var fixedNow = time.Date(2026, 1, 2, 3, 4, 5, 0, time.Local)

// writeFixture 在 root 下建一个文件（自动建父目录），返回其绝对路径。
func writeFixture(t *testing.T, root, rel, content string) string {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("建父目录失败: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写夹具失败: %v", err)
	}
	return path
}

// TestNextLogArchivePathRequiresName 锁定「路径没有名字」时的 ValueError 语义。
//
// Python 的 PurePath.with_name 对空路径、"."、"/" 都会抛 ValueError；Go 侧对应
// ErrEmptyName，且必须在碰文件系统之前就返回（"/" 下的候选名是不可写的）。
func TestNextLogArchivePathRequiresName(t *testing.T) {
	for _, path := range []string{"", ".", "/", "///"} {
		if _, err := NextLogArchivePath(path, fixedNow); !errors.Is(err, ErrEmptyName) {
			t.Errorf("NextLogArchivePath(%q) 错误 = %v，期望 ErrEmptyName", path, err)
		}
	}
}

// TestNextLogArchivePathSuffixFallback 锁定后缀规则（log_files.py:21）。
//
// `path.suffix or ".log"`：后缀取最后一个点之后的部分，但结尾的点与开头的点都
// 不算后缀（PurePath 的规则），于是 "server"、".hidden"、"server." 都回退成 .log，
// 而 "archive.tar.gz" 用的是 ".gz"。
func TestNextLogArchivePathSuffixFallback(t *testing.T) {
	root := t.TempDir()
	cases := map[string]string{
		"logs/server.log":     "logs/server.20260102-030405.log",
		"logs/server":         "logs/server.20260102-030405.log",
		"logs/server.":        "logs/server..20260102-030405.log",
		"logs/.hidden":        "logs/.hidden.20260102-030405.log",
		"logs/archive.tar.gz": "logs/archive.tar.20260102-030405.gz",
		"logs/x.Server.LOG":   "logs/x.Server.20260102-030405.LOG",
		"logs/dir.d/log":      "logs/dir.d/log.20260102-030405.log",
	}
	for arg, want := range cases {
		got, err := NextLogArchivePath(filepath.Join(root, filepath.FromSlash(arg)), fixedNow)
		if err != nil {
			t.Fatalf("NextLogArchivePath(%q) 报错: %v", arg, err)
		}
		rel, err := filepath.Rel(root, got)
		if err != nil {
			t.Fatalf("计算相对路径失败: %v", err)
		}
		if filepath.ToSlash(rel) != want {
			t.Errorf("NextLogArchivePath(%q) = %q，期望 %q", arg, filepath.ToSlash(rel), want)
		}
	}
}

// TestNextLogArchivePathCollisionIndex 锁定同名冲突时的编号规则。
//
// 编号从 1 开始，且判定用的是「候选是否存在」（目录也算存在）。
func TestNextLogArchivePathCollisionIndex(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "server.log")
	want := func(index string) string {
		return filepath.Join(root, "server.20260102-030405"+index+".log")
	}
	// 占用 .log 与 .1.log，下一个必须是 .2.log。
	writeFixture(t, root, "server.20260102-030405.log", "old")
	writeFixture(t, root, "server.20260102-030405.1.log", "old")
	got, err := NextLogArchivePath(target, fixedNow)
	if err != nil {
		t.Fatalf("NextLogArchivePath 报错: %v", err)
	}
	if got != want(".2") {
		t.Fatalf("归档路径 = %q，期望 %q", got, want(".2"))
	}
	// 目录同样算「已存在」。
	if err := os.Mkdir(want(".2"), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	got, err = NextLogArchivePath(target, fixedNow)
	if err != nil {
		t.Fatalf("NextLogArchivePath 报错: %v", err)
	}
	if got != want(".3") {
		t.Fatalf("目录占位时归档路径 = %q，期望 %q", got, want(".3"))
	}
}

// TestArchiveCurrentLogRotatesOnlyNonEmpty 锁定「长度为 0 不归档」这条分支
// （log_files.py:10）。
//
// 空文件（或压根不存在的文件）不进归档，只保证原路径存在且为空；非空文件才
// 改名归档并在原路径重建空文件。
func TestArchiveCurrentLogRotatesOnlyNonEmpty(t *testing.T) {
	root := t.TempDir()

	empty := writeFixture(t, root, "case/empty.log", "")
	archive, archived, err := ArchiveCurrentLog(empty, fixedNow)
	if err != nil {
		t.Fatalf("空文件归档报错: %v", err)
	}
	if archived || archive != "" {
		t.Fatalf("空文件不该归档，实际 archived=%v path=%q", archived, archive)
	}

	full := writeFixture(t, root, "case/full.log", "第一行\n第二行\n")
	archive, archived, err = ArchiveCurrentLog(full, fixedNow)
	if err != nil {
		t.Fatalf("非空文件归档报错: %v", err)
	}
	if !archived {
		t.Fatal("非空文件应当归档")
	}
	wantArchive := filepath.Join(root, "case", "full.20260102-030405.log")
	if archive != wantArchive {
		t.Fatalf("归档路径 = %q，期望 %q", archive, wantArchive)
	}
	// 归档文件保留原内容，原路径变成空文件。
	archivedText, err := os.ReadFile(archive)
	if err != nil {
		t.Fatalf("读取归档失败: %v", err)
	}
	if string(archivedText) != "第一行\n第二行\n" {
		t.Errorf("归档内容 = %q，期望原内容", string(archivedText))
	}
	info, err := os.Stat(full)
	if err != nil {
		t.Fatalf("原路径应被重建: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("原路径长度 = %d，期望 0", info.Size())
	}
}

// TestArchiveCurrentLogCreatesParents 锁定「父目录按需创建」。
func TestArchiveCurrentLogCreatesParents(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "a", "b", "c", "app.log")
	archive, archived, err := ArchiveCurrentLog(target, fixedNow)
	if err != nil {
		t.Fatalf("归档报错: %v", err)
	}
	if archived || archive != "" {
		t.Fatalf("文件不存在时不该归档，实际 archived=%v path=%q", archived, archive)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("原路径应被创建: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("新建文件长度 = %d，期望 0", info.Size())
	}
}

// TestArchivedLogPathsSortedByNameDescending 锁定返回顺序（log_files.py:36）。
//
// 按文件名倒序（不是按时间），且只保留普通文件。
func TestArchivedLogPathsSortedByNameDescending(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "case/server.log", "current")
	writeFixture(t, root, "case/server.20260101-000000.log", "a")
	writeFixture(t, root, "case/server.20260102-000000.log", "b")
	writeFixture(t, root, "case/server.20260102-000000.1.log", "c")
	writeFixture(t, root, "case/other.20260102-000000.log", "d")
	if err := os.Mkdir(filepath.Join(root, "case", "server.dir.20260101-000000.log"), 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}

	got := ArchivedLogPaths(filepath.Join(root, "case", "server.log"))
	want := []string{
		"server.20260102-000000.log",
		"server.20260102-000000.1.log",
		"server.20260101-000000.log",
	}
	if len(got) != len(want) {
		t.Fatalf("归档数 = %d，期望 %d（实际 %v）", len(got), len(want), got)
	}
	for index, item := range got {
		if filepath.Base(item) != want[index] {
			t.Fatalf("第 %d 个归档 = %q，期望 %q", index, filepath.Base(item), want[index])
		}
	}
}

// TestArchivedLogPathsExcludesSelf 锁定「日志文件自身被排除」。
//
// 归档文件的名字同样匹配 `stem.*后缀` 模式，Python 用 `candidate != path` 排除，
// Go 侧按归一化后的字符串比较。
func TestArchivedLogPathsExcludesSelf(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "server.20260101-000000.log", "a")
	got := ArchivedLogPaths(filepath.Join(root, "server.20260101-000000.log"))
	if len(got) != 0 {
		t.Fatalf("归档 = %v，期望空", got)
	}
}

// TestArchivedLogPathsSwallowsFilesystemErrors 锁定「读不到就是空列表」。
//
// Python 的 pathlib 在父目录不存在或不是目录时吞掉 OSError 返回空；Go 侧显式
// 忽略 ReadDir/Stat 的错误。
func TestArchivedLogPathsSwallowsFilesystemErrors(t *testing.T) {
	root := t.TempDir()
	missing := ArchivedLogPaths(filepath.Join(root, "nope", "server.log"))
	if len(missing) != 0 {
		t.Fatalf("父目录不存在时归档 = %v，期望空", missing)
	}
	blocked := writeFixture(t, root, "blocked.log", "i am a file")
	got := ArchivedLogPaths(filepath.Join(blocked, "server.log"))
	if len(got) != 0 {
		t.Fatalf("父路径是文件时归档 = %v，期望空", got)
	}
}

// TestArchivedLogPathsGlobFlavorDivergesFromPython 记录一处刻意的差异：
// 通配符引擎不同。
//
// pathlib 用 fnmatch（`[!b]` 是**取反**字符类，且没有转义字符），Go 用
// filepath.Match（取反写 `[^b]`，`\` 是转义）。日志文件名里出现 `[` 不现实，
// 为这点差异手写 fnmatch 移植不值得，因此这里把差异写进测试而不是假装一致：
//
//   - 用例：日志文件 a[!b].log，同名归档模式 `a[!b].*.log`；
//   - Python（fnmatch）匹配 ac.*.log（取反）；
//   - Go（filepath.Match）匹配 ab.*.log（`[!b]` 里的 ! 只是普通字符）。
//
// 反过来的空区间 `[z-a]`：Python 视为永不匹配，Go 报 ErrBadPattern 后同样不匹配，
// 结果一致（下面一并断言）。
func TestArchivedLogPathsGlobFlavorDivergesFromPython(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "ab.20260101-000000.log", "a")
	writeFixture(t, root, "ac.20260101-000000.log", "b")
	writeFixture(t, root, "a!.20260101-000000.log", "c")

	got := ArchivedLogPaths(filepath.Join(root, "a[!b].log"))
	names := make([]string, 0, len(got))
	for _, item := range got {
		names = append(names, filepath.Base(item))
	}
	// Go 的语义：`[!b]` 匹配 '!' 或 'b'，所以命中 ab 与 a!，不命中 ac。
	want := []string{"ab.20260101-000000.log", "a!.20260101-000000.log"}
	if len(names) != len(want) {
		t.Fatalf("归档 = %v，期望 %v（Python 的 fnmatch 会给出 ac.…log）", names, want)
	}
	for index, name := range names {
		if name != want[index] {
			t.Fatalf("第 %d 个归档 = %q，期望 %q", index, name, want[index])
		}
	}

	emptyRange := ArchivedLogPaths(filepath.Join(root, "a[z-a].log"))
	if len(emptyRange) != 0 {
		t.Fatalf("空区间 [z-a] 应为空，实际 %v", emptyRange)
	}
}

// TestArchivedLogPathsIsCaseSensitiveOnAllPlatforms 记录另一处刻意差异：
// 大小写敏感性。
//
// pathlib 在 Windows 上 glob 不区分大小写（fnmatch 前会 normcase），Go 的
// filepath.Match 在所有平台都区分。这里固定 Go 的行为：若跟着 Windows 变成
// 不区分，同一份语料在 CI（Linux）与开发机（Windows）上会得到不同期望值。
//
// 注意不能在同一个目录里放「只差大小写」的两个文件：Windows 文件系统本身
// 不区分大小写，第二次写入会落到同一个文件上，测出来的现象是假的。
func TestArchivedLogPathsIsCaseSensitiveOnAllPlatforms(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, "SERVER.20260101-000000.LOG", "a")
	// 模式来自小写的日志文件名 server.log → `server.*.log`，不该匹配大写归档。
	if got := ArchivedLogPaths(filepath.Join(root, "server.log")); len(got) != 0 {
		t.Fatalf("归档 = %v，期望空（Go 侧一律区分大小写）", got)
	}
	// 换个大小写一致的 stem，证明上面的空结果不是因为「根本找不到文件」。
	writeFixture(t, root, "app.20260101-000000.log", "b")
	got := ArchivedLogPaths(filepath.Join(root, "app.log"))
	if len(got) != 1 || filepath.Base(got[0]) != "app.20260101-000000.log" {
		t.Fatalf("大小写一致时归档 = %v，期望命中 app.20260101-000000.log", got)
	}
}
