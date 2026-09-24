package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 本文件覆盖自更新里最容易悄悄坏掉的三处：产物命名（与 release.yml 必须逐字一致）、
// 校验（失败必须拒绝安装）、替换（顺序不可交换）。

// withReleasesBaseURL 把下载根地址临时指向假服务，返回恢复函数（配合 defer 使用）。
//
// 这些测试用不着真去 GitHub 下载十几兆；更重要的是**离线可测**——CI 上并没有代理。
func withReleasesBaseURL(url string) func() {
	original := releasesBaseURL
	releasesBaseURL = url
	return func() { releasesBaseURL = original }
}

// TestAssetNameMatchesReleaseWorkflow 锁定产物命名与 .github/workflows/release.yml 一致。
//
// 这两处一旦漂移，自更新会去下一个不存在的文件，表现为 404——而用户看到的是"更新失败"，
// 根因却在发布流水线里。因此把 release.yml 的命名规则**逐字抄进测试**作为对照。
func TestAssetNameMatchesReleaseWorkflow(t *testing.T) {
	// release.yml:125 的 `-o "dist/amkr_${VERSION}_$1_$2${EXT}"`，EXT 在 windows 上为 .exe。
	cases := []struct {
		goos, goarch, want string
	}{
		{"windows", "amd64", "amkr_5.1.1_windows_amd64.exe"},
		{"windows", "arm64", "amkr_5.1.1_windows_arm64.exe"},
		{"linux", "amd64", "amkr_5.1.1_linux_amd64"},
		{"linux", "arm64", "amkr_5.1.1_linux_arm64"},
		{"darwin", "amd64", "amkr_5.1.1_darwin_amd64"},
		{"darwin", "arm64", "amkr_5.1.1_darwin_arm64"},
	}
	for _, tc := range cases {
		if got := AssetName("5.1.1", tc.goos, tc.goarch); got != tc.want {
			t.Errorf("AssetName(5.1.1, %s, %s) = %q，期望 %q", tc.goos, tc.goarch, got, tc.want)
		}
	}
}

// TestReleaseWorkflowMatrixMatchesAssetName 反向锁定：flow 里声明的每个矩阵项都能被
// AssetName 覆盖。矩阵改了而这里没跟上时，用户在那个平台上就更新不了。
func TestReleaseWorkflowMatrixMatchesAssetName(t *testing.T) {
	// 与 release.yml 的 build matrix 对应（6 个平台）。
	matrix := [][2]string{
		{"windows", "amd64"}, {"windows", "arm64"},
		{"linux", "amd64"}, {"linux", "arm64"},
		{"darwin", "amd64"}, {"darwin", "arm64"},
	}
	for _, entry := range matrix {
		asset := AssetName("9.9.9", entry[0], entry[1])
		if !strings.HasPrefix(asset, "amkr_9.9.9_") {
			t.Errorf("%s/%s: 产物名 %q 前缀不对", entry[0], entry[1], asset)
		}
		if entry[0] == "windows" && !strings.HasSuffix(asset, ".exe") {
			t.Errorf("windows 产物必须以 .exe 结尾，实际 %q", asset)
		}
		if entry[0] != "windows" && strings.HasSuffix(asset, ".exe") {
			t.Errorf("非 windows 产物不应带 .exe，实际 %q", asset)
		}
	}
}

// TestDownloadAndChecksumsURL 锁定地址形状（标签带 v 前缀）。
func TestDownloadAndChecksumsURL(t *testing.T) {
	asset := "amkr_5.1.1_windows_amd64.exe"
	want := "https://github.com/Sparrived/auto-model-key-router/releases/download/v5.1.1/" + asset
	if got := DownloadURL("5.1.1", asset); got != want {
		t.Errorf("DownloadURL = %q，期望 %q", got, want)
	}
	wantChecksums := "https://github.com/Sparrived/auto-model-key-router/releases/download/v5.1.1/checksums.txt"
	if got := ChecksumsURL("5.1.1"); got != wantChecksums {
		t.Errorf("ChecksumsURL = %q，期望 %q", got, wantChecksums)
	}
}

// TestParseChecksums 覆盖 sha256sum 的两种行格式与"找不到"。
func TestParseChecksums(t *testing.T) {
	text := strings.Join([]string{
		"1111111111111111111111111111111111111111111111111111111111111111  amkr_5.1.1_linux_amd64",
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA *amkr_5.1.1_windows_amd64.exe",
		"",
		"不是一行校验和",
	}, "\n")

	// 文本模式（两个空格）。
	if got, ok := ParseChecksums(text, "amkr_5.1.1_linux_amd64"); !ok || got != strings.Repeat("1", 64) {
		t.Errorf("文本模式解析 = %q/%v", got, ok)
	}
	// 二进制模式（`*` 前缀）：必须剥掉星号再比对，否则这条永远匹配不上。
	if got, ok := ParseChecksums(text, "amkr_5.1.1_windows_amd64.exe"); !ok || got != strings.Repeat("a", 64) {
		t.Errorf("二进制模式解析 = %q/%v（应剥掉 * 并小写化）", got, ok)
	}
	// 找不到必须返回 false——调用方据此拒绝安装。
	if _, ok := ParseChecksums(text, "amkr_5.1.1_darwin_arm64"); ok {
		t.Error("不存在的产物应当解析失败")
	}
}

// TestVerifySHA256RejectsMismatch 锁定"内容不符必须报错"。
func TestVerifySHA256RejectsMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "payload")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("hello"))
	good := hex.EncodeToString(sum[:])

	if err := VerifySHA256(path, good); err != nil {
		t.Errorf("校验和相符却报错: %v", err)
	}
	// 大小写不敏感：checksums.txt 由不同工具产出时大小写不定。
	if err := VerifySHA256(path, strings.ToUpper(good)); err != nil {
		t.Errorf("大写校验和应当接受: %v", err)
	}
	if err := VerifySHA256(path, strings.Repeat("0", 64)); err == nil {
		t.Error("校验和不符必须报错")
	}
}

// TestApplyRefusesInstallOnChecksumMismatch 是本包最重要的一条断言：
// **校验不通过时，已安装的二进制必须原封不动**。
func TestApplyRefusesInstallOnChecksumMismatch(t *testing.T) {
	const asset = "amkr_9.9.9_test_arch"
	payload := []byte("这是伪造的新版本内容")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/checksums.txt"):
			// 故意给一个与 payload 不符的校验和。
			_, _ = w.Write([]byte(strings.Repeat("0", 64) + "  " + asset + "\n"))
		case strings.HasSuffix(r.URL.Path, "/"+asset):
			_, _ = w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	defer withReleasesBaseURL(server.URL)()

	dir := t.TempDir()
	executable := filepath.Join(dir, "amkr.exe")
	original := []byte("我是原来的可执行文件")
	if err := os.WriteFile(executable, original, 0o755); err != nil {
		t.Fatal(err)
	}

	stale, err := Apply(server.Client(), "9.9.9", asset, dir, executable)
	if err == nil {
		t.Fatal("校验和不符时 Apply 必须报错")
	}
	if !strings.Contains(err.Error(), "sha256") {
		t.Errorf("错误应点明校验失败，实际 %v", err)
	}
	if stale != "" {
		t.Errorf("失败时不应返回 stale 路径，实际 %q", stale)
	}

	// 关键断言：原文件内容与位置都没变，且没有留下任何暂存/旧文件。
	got, readErr := os.ReadFile(executable)
	if readErr != nil {
		t.Fatalf("原可执行文件不见了: %v", readErr)
	}
	if string(got) != string(original) {
		t.Errorf("原可执行文件被改动：%q", got)
	}
	for _, leftover := range []string{StalePath(executable), filepath.Join(dir, exeName())} {
		if _, statErr := os.Stat(leftover); statErr == nil {
			t.Errorf("失败后不应留下 %s", leftover)
		}
	}
}

// TestApplyInstallsVerifiedPayload 走通一次成功路径，并确认旧文件被挪到 .old。
func TestApplyInstallsVerifiedPayload(t *testing.T) {
	const asset = "amkr_9.9.9_test_arch"
	payload := []byte("新版本内容")
	sum := sha256.Sum256(payload)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/checksums.txt") {
			_, _ = w.Write([]byte(hex.EncodeToString(sum[:]) + "  " + asset + "\n"))
			return
		}
		_, _ = w.Write(payload)
	}))
	defer server.Close()
	defer withReleasesBaseURL(server.URL)()

	dir := t.TempDir()
	executable := filepath.Join(dir, "amkr.exe")
	if err := os.WriteFile(executable, []byte("旧版本内容"), 0o755); err != nil {
		t.Fatal(err)
	}

	stale, err := Apply(server.Client(), "9.9.9", asset, dir, executable)
	if err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}
	if stale != StalePath(executable) {
		t.Errorf("stale = %q，期望 %q", stale, StalePath(executable))
	}
	got, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Errorf("安装后内容 = %q，期望 %q", got, payload)
	}
	// 旧文件被挪到 .old 而不是删除（Windows 上此刻还删不掉——它可能正被运行）。
	old, err := os.ReadFile(stale)
	if err != nil {
		t.Fatalf("旧文件应被挪到 %s: %v", stale, err)
	}
	if string(old) != "旧版本内容" {
		t.Errorf("旧文件内容 = %q", old)
	}
	// 暂存文件已被消费掉。
	if _, err := os.Stat(filepath.Join(dir, exeName())); err == nil {
		t.Error("暂存文件应当已被 rename 消费")
	}
}

// TestReplaceRestoresTargetWhenSecondRenameFails 锁定回滚：
// 「挪开旧的」成功而「放入新的」失败时，必须把旧的改回来，
// 否则用户会停在一个**没有可执行文件**的状态。
func TestReplaceRestoresTargetWhenSecondRenameFails(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "amkr")
	if err := os.WriteFile(executable, []byte("旧"), 0o755); err != nil {
		t.Fatal(err)
	}
	// staged 指向一个**不存在**的路径：第二次 rename 在两个平台上都必然失败
	// （源不存在）。这比"目标不可写"更可靠——后者在 Windows 上要依赖目录占用等
	// 平台细节，而目录改名又可能因为目标是新名字而意外成功。
	staged := filepath.Join(dir, "并不存在的暂存文件")

	if _, err := Replace(executable, staged); err == nil {
		t.Fatal("第二次 rename 失败时 Replace 必须报错")
	}
	got, err := os.ReadFile(executable)
	if err != nil {
		t.Fatalf("回滚失败：目标文件不见了（%v）", err)
	}
	if string(got) != "旧" {
		t.Errorf("回滚后内容 = %q，期望 旧", got)
	}
}

// TestStageRejectsMissingSource 锁定"源文件不存在时报错而不是留下空文件"。
func TestStageRejectsMissingSource(t *testing.T) {
	dir := t.TempDir()
	if _, err := Stage(filepath.Join(dir, "并不存在"), dir); err == nil {
		t.Error("源文件不存在时 Stage 必须报错")
	}
	if _, err := os.Stat(filepath.Join(dir, exeName())); err == nil {
		t.Error("失败时不应留下暂存文件")
	}
}

// TestStalePathIsSuffixOfTarget 锁定 .old 是 target 的后缀（清理逻辑依赖这个形状）。
func TestStalePathIsSuffixOfTarget(t *testing.T) {
	target := filepath.Join("some", "dir", "amkr.exe")
	if got := StalePath(target); got != target+".old" {
		t.Errorf("StalePath = %q", got)
	}
	if !strings.HasSuffix(StalePath(target), StaleSuffix) {
		t.Error("StalePath 必须以 StaleSuffix 结尾")
	}
}

// noRetrySleep 把重试之间的等待换成空操作，返回恢复函数。
//
// 没有它，每个失败用例都要真等 500ms+1s+2s=3.5 秒。
func noRetrySleep() func() {
	original := retrySleep
	retrySleep = func(time.Duration) {}
	return func() { retrySleep = original }
}

// TestDownloadRetriesTransientFailures 是本次修复的核心断言。
//
// 起因是一次真实的失败：`amkr --update` 死在
// `TLS handshake timeout`。原来的实现只做一次 client.Get，**任何**一次传输层抖动都会
// 让整个更新失败——而这类抖动重试一次就好了。这里用「前两次连接被重置、第三次成功」
// 模拟那个抖动，断言最终能拿到内容。
func TestDownloadRetriesTransientFailures(t *testing.T) {
	defer noRetrySleep()

	payload := []byte("新版本内容")
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) <= 2 {
			// 模拟传输层错误：直接掐断连接（客户端会看到 EOF/连接重置）。
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("假服务不支持 Hijack")
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("Hijack 失败: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "payload")
	if err := Download(server.Client(), server.URL, path); err != nil {
		t.Fatalf("瞬时失败应当重试并最终成功，实际 %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Errorf("内容 = %q，期望 %q", got, payload)
	}
	if n := atomic.LoadInt32(&attempts); n != 3 {
		t.Errorf("尝试次数 = %d，期望 3（前两次失败后成功）", n)
	}
}

// TestDownloadRetriesServerErrorsButNotMissingAsset 锁定重试的**边界**。
//
// 5xx 值得再试（GitHub 偶发）；404 是确定性结论——产物名与 release.yml 漂移、或标签
// 还没发布。对 404 重试只会让用户多等 3.5 秒再看到同一句话，因此必须立刻返回。
func TestDownloadRetriesServerErrorsButNotMissingAsset(t *testing.T) {
	defer noRetrySleep()

	t.Run("503 重试到成功", func(t *testing.T) {
		var attempts int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if atomic.AddInt32(&attempts, 1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte("ok"))
		}))
		defer server.Close()

		path := filepath.Join(t.TempDir(), "payload")
		if err := Download(server.Client(), server.URL, path); err != nil {
			t.Fatalf("503 应当重试，实际 %v", err)
		}
		if n := atomic.LoadInt32(&attempts); n != 2 {
			t.Errorf("尝试次数 = %d，期望 2", n)
		}
	})

	t.Run("404 不重试", func(t *testing.T) {
		var attempts int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&attempts, 1)
			http.NotFound(w, r)
		}))
		defer server.Close()

		path := filepath.Join(t.TempDir(), "payload")
		err := Download(server.Client(), server.URL, path)
		if err == nil {
			t.Fatal("404 必须报错")
		}
		if !strings.Contains(err.Error(), "404") {
			t.Errorf("错误应点明 404，实际 %v", err)
		}
		if n := atomic.LoadInt32(&attempts); n != 1 {
			t.Errorf("404 不该重试，实际请求了 %d 次", n)
		}
	})
}

// TestDownloadFailsAfterExhaustingRetries 锁定「重试有上限」：一直失败就如实报错，
// 不能无限重试把用户挂在那里。
func TestDownloadFailsAfterExhaustingRetries(t *testing.T) {
	defer noRetrySleep()

	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "payload")
	if err := Download(server.Client(), server.URL, path); err == nil {
		t.Fatal("持续 500 时 Download 必须报错")
	}
	if n := atomic.LoadInt32(&attempts); n != downloadAttempts {
		t.Errorf("尝试次数 = %d，期望 %d（有上限）", n, downloadAttempts)
	}
	// 失败后不该留下半截文件。
	if _, err := os.Stat(path); err == nil {
		t.Error("失败后不该留下下载文件")
	}
}

// TestFetchChecksumsRetriesAndKeepsDeterministicFailure 锁定校验和路径的两面：
// 网络抖动要重试，而「文件里没有这个产物」是确定性结论、不该重试。
func TestFetchChecksumsRetriesAndKeepsDeterministicFailure(t *testing.T) {
	defer noRetrySleep()

	const asset = "amkr_9.9.9_windows_amd64.exe"
	t.Run("瞬时失败后成功", func(t *testing.T) {
		var attempts int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if atomic.AddInt32(&attempts, 1) == 1 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			_, _ = w.Write([]byte(strings.Repeat("a", 64) + "  " + asset + "\n"))
		}))
		defer server.Close()
		defer withReleasesBaseURL(server.URL)()

		digest, err := FetchChecksums(server.Client(), "9.9.9", asset)
		if err != nil {
			t.Fatalf("瞬时失败应当重试，实际 %v", err)
		}
		if digest != strings.Repeat("a", 64) {
			t.Errorf("digest = %q", digest)
		}
	})

	t.Run("缺少产物不重试", func(t *testing.T) {
		var attempts int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&attempts, 1)
			_, _ = w.Write([]byte(strings.Repeat("a", 64) + "  amkr_9.9.9_别的平台\n"))
		}))
		defer server.Close()
		defer withReleasesBaseURL(server.URL)()

		if _, err := FetchChecksums(server.Client(), "9.9.9", asset); err == nil {
			t.Fatal("校验和里没有该产物时必须报错")
		}
		if n := atomic.LoadInt32(&attempts); n != 1 {
			t.Errorf("确定性结论不该重试，实际请求了 %d 次", n)
		}
	})
}

// TestFetchWithRetryReturnsConsumeErrorImmediately 锁定：consume 的错误（磁盘写不进去）
// 不重试——它不是网络问题，重试也不会变好。
func TestFetchWithRetryReturnsConsumeErrorImmediately(t *testing.T) {
	defer noRetrySleep()

	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		_, _ = w.Write([]byte("body"))
	}))
	defer server.Close()

	sentinel := errors.New("磁盘满了")
	err := fetchWithRetry(server.Client(), server.URL, func(io.Reader) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Errorf("错误应为 consume 的原错误，实际 %v", err)
	}
	if n := atomic.LoadInt32(&attempts); n != 1 {
		t.Errorf("consume 失败不该重试，实际请求了 %d 次", n)
	}
}
