package logfiles

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// corpusFile 是 scripts/gen_logfiles_corpus.py 产出的对拍语料。
//
// 夹具路径与期望值全部是**相对于临时根**的 POSIX 路径：语料在 Windows 上生成、
// 在 Linux CI 上重放，只有相对路径才与机器无关。
type corpusFile struct {
	Cases []corpusCase `json:"cases"`
}

// fixtureEntry 是语料里的一条夹具（目录或文件）。
type fixtureEntry struct {
	Path string `json:"path"`
	Text string `json:"text"`
	Dir  bool   `json:"dir"`
}

// treeEntry 是归档操作后的一个文件。
type treeEntry struct {
	Path string `json:"path"`
	Text string `json:"text"`
}

// corpusCase 是一条用例。
type corpusCase struct {
	Name    string         `json:"name"`
	Op      string         `json:"op"`
	Arg     string         `json:"arg"`
	Now     string         `json:"now"`
	Fixture []fixtureEntry `json:"fixture"`
	Note    string         `json:"note"`
	// Want 是 next_log_archive_path 的结果，或 archive_current_log 的归档路径。
	Want string `json:"want"`
	// WantArchived 是 archive_current_log 的返回值是否为 Some。
	WantArchived bool `json:"want_archived"`
	// WantPaths 是 archived_log_paths 的结果（已按返回顺序）。
	WantPaths []string `json:"want_paths"`
	// WantTree 是 archive_current_log 执行后的文件树。
	WantTree []treeEntry `json:"want_tree"`
	// WantError 表示 Python 抛异常；Go 侧只要返回错误即可（异常类名随平台变化）。
	WantError bool `json:"want_error"`
}

// loadCorpus 读取对拍语料。
func loadCorpus(t *testing.T) corpusFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "logfiles_corpus.json"))
	if err != nil {
		t.Fatalf("读取语料失败: %v", err)
	}
	var corpus corpusFile
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("解析语料失败: %v", err)
	}
	if len(corpus.Cases) == 0 {
		t.Fatal("语料为空")
	}
	return corpus
}

// materialize 按语料在 root 下铺夹具。
func materialize(t *testing.T, root string, fixture []fixtureEntry) {
	t.Helper()
	for _, entry := range fixture {
		target := filepath.Join(root, filepath.FromSlash(entry.Path))
		if entry.Dir {
			if err := os.MkdirAll(target, 0o755); err != nil {
				t.Fatalf("建目录 %s 失败: %v", entry.Path, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatalf("建父目录 %s 失败: %v", entry.Path, err)
		}
		// 用字节写入，避免平台换行翻译把语料内容变形。
		if err := os.WriteFile(target, []byte(entry.Text), 0o644); err != nil {
			t.Fatalf("写夹具 %s 失败: %v", entry.Path, err)
		}
	}
}

// resolveArg 把语料里的相对 arg 拼到临时根下。
//
// 以 "/" 开头或为空的 arg 原样使用：这两种输入在 Python 里都是「没有名字的路径」，
// 被测函数会在碰文件系统之前就报错，因此不需要（也不能）落到临时目录里。
func resolveArg(root, arg string) string {
	if arg == "" || strings.HasPrefix(arg, "/") {
		return arg
	}
	return filepath.Join(root, filepath.FromSlash(arg))
}

// relativeToRoot 把结果转成与语料同形的相对 POSIX 路径。
func relativeToRoot(t *testing.T, root, path string) string {
	t.Helper()
	rel, err := filepath.Rel(root, path)
	if err != nil {
		t.Fatalf("计算相对路径失败: %v", err)
	}
	return filepath.ToSlash(rel)
}

// snapshot 复刻生成器里的文件树快照：普通文件 + 文本内容，按相对路径排序。
func snapshot(t *testing.T, root string) []treeEntry {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历夹具失败: %v", err)
	}
	sort.Slice(files, func(i, j int) bool {
		return relativeToRoot(t, root, files[i]) < relativeToRoot(t, root, files[j])
	})
	result := make([]treeEntry, 0, len(files))
	for _, path := range files {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", path, err)
		}
		result = append(result, treeEntry{
			Path: relativeToRoot(t, root, path),
			Text: string(content),
		})
	}
	return result
}

// parseNow 解析语料里的注入时刻（形如 2026-01-02T03:04:05，按本地时区解读）。
//
// 语料里没有 now 字段的用例表示「用不到时刻」（archived_log_paths 与报错路径），
// 此时返回零值即可。
func parseNow(t *testing.T, text string) time.Time {
	t.Helper()
	if text == "" {
		return time.Time{}
	}
	moment, err := time.ParseInLocation("2006-01-02T15:04:05", text, time.Local)
	if err != nil {
		t.Fatalf("解析时刻 %q 失败: %v", text, err)
	}
	return moment
}

// TestLogFilesMatchesPython 重放语料，逐条断言与参照实现一致。
func TestLogFilesMatchesPython(t *testing.T) {
	corpus := loadCorpus(t)
	var assertions int

	for _, testCase := range corpus.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			root := t.TempDir()
			materialize(t, root, testCase.Fixture)
			arg := resolveArg(root, testCase.Arg)
			now := parseNow(t, testCase.Now)

			switch testCase.Op {
			case "next_log_archive_path":
				got, err := NextLogArchivePath(arg, now)
				assertions++
				if testCase.WantError {
					if err == nil {
						t.Fatalf("期望报错（Python 抛 ValueError），实际得到 %q", got)
					}
					if !errors.Is(err, ErrEmptyName) {
						t.Fatalf("期望 ErrEmptyName，实际 %v", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("参照成功但 Go 报错: %v", err)
				}
				if want := relativeToRoot(t, root, got); want != testCase.Want {
					t.Fatalf("归档路径 = %q，期望 %q", want, testCase.Want)
				}

			case "archive_current_log":
				got, archived, err := ArchiveCurrentLog(arg, now)
				assertions++
				if testCase.WantError {
					if err == nil {
						t.Fatalf("期望报错（父路径被占位），实际得到 %q", got)
					}
					return
				}
				if err != nil {
					t.Fatalf("参照成功但 Go 报错: %v", err)
				}
				if archived != testCase.WantArchived {
					t.Fatalf("archived = %v，期望 %v", archived, testCase.WantArchived)
				}
				// 未归档时 Python 返回 None，Go 返回空串：空串不做相对路径换算。
				gotArchive := ""
				if got != "" {
					gotArchive = relativeToRoot(t, root, got)
				}
				if gotArchive != testCase.Want {
					t.Fatalf("归档路径 = %q，期望 %q", gotArchive, testCase.Want)
				}
				gotTree := snapshot(t, root)
				if len(gotTree) != len(testCase.WantTree) {
					t.Fatalf("文件数 = %d，期望 %d（Go=%v）",
						len(gotTree), len(testCase.WantTree), gotTree)
				}
				for index, want := range testCase.WantTree {
					if gotTree[index].Path != want.Path || gotTree[index].Text != want.Text {
						t.Fatalf("第 %d 个文件 = {%q, %q}，期望 {%q, %q}",
							index, gotTree[index].Path, gotTree[index].Text,
							want.Path, want.Text)
					}
				}

			case "archived_log_paths":
				got := ArchivedLogPaths(arg)
				assertions++
				gotPaths := make([]string, 0, len(got))
				for _, path := range got {
					gotPaths = append(gotPaths, relativeToRoot(t, root, path))
				}
				if len(gotPaths) != len(testCase.WantPaths) {
					t.Fatalf("归档数 = %d，期望 %d（Go=%v，期望 %v）",
						len(gotPaths), len(testCase.WantPaths), gotPaths, testCase.WantPaths)
				}
				for index, want := range testCase.WantPaths {
					if gotPaths[index] != want {
						t.Fatalf("第 %d 个归档 = %q，期望 %q", index, gotPaths[index], want)
					}
				}

			default:
				t.Fatalf("未知操作: %s", testCase.Op)
			}
		})
	}
	t.Logf("共重放 %d 个断言", assertions)
}
