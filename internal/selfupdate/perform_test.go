package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Sparrived/auto-model-key-router/internal/updatecheck"
)

// 本文件覆盖 Perform 的编排：何时该换文件、何时该启动助手、失败时怎么报告。
//
// 成功路径需要一个能真的下载的地方，因此这里把 releasesBaseURL 指向 httptest 假服务
// （见下面 withFakeReleases 的说明）。

// stubCheck 造一个固定的版本检查结果：latest 为空表示"查不到/没有新版"。
func stubCheck(current, latest string) func() updatecheck.Result {
	return func() updatecheck.Result {
		result := updatecheck.Result{CurrentVersion: current}
		if latest != "" {
			value := latest
			result.LatestVersion = &value
		}
		return result
	}
}

// withFakeReleases 把下载根地址指向假 release 服务，并返回该地址。
//
// 用变量而不是接缝函数：生产代码只该有一个地址来源，多一个参数就多一处能漂移的地方；
// 而测试改写它完全安全（同包、串行测试，t.Cleanup 恢复）。
func withFakeReleases(t *testing.T, asset string, payload []byte) *httptest.Server {
	t.Helper()
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])

	var served bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/checksums.txt"):
			_, _ = w.Write([]byte(digest + "  " + asset + "\n"))
		case strings.HasSuffix(r.URL.Path, "/"+asset):
			served = true
			_, _ = w.Write(payload)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	// served 只是"这个假服务确实被打过"的证据，失败时由测试自己 report。
	t.Cleanup(func() {
		if !served {
			t.Log("假 release 服务未被访问（可能是地址没被替换生效）")
		}
	})
	t.Cleanup(withReleasesBaseURL(server.URL))
	return server
}

// newInstalledBinary 造一个假的"已安装的 amkr"。
func newInstalledBinary(t *testing.T, content string) (dir, executable string) {
	t.Helper()
	dir = t.TempDir()
	executable = filepath.Join(dir, "amkr.exe")
	if err := os.WriteFile(executable, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, executable
}

// TestPerformNoOpWhenAlreadyLatest 锁定"已是最新"时既不下载也不启助手。
func TestPerformNoOpWhenAlreadyLatest(t *testing.T) {
	spawned := false
	result, err := Perform(Options{
		Check:      stubCheck("5.1.1", "5.1.1"),
		Executable: filepath.Join(t.TempDir(), "amkr.exe"),
		Spawn: func([]string, string) (int, error) {
			spawned = true
			return 1, nil
		},
	})
	if err != nil {
		t.Fatalf("Perform 报错: %v", err)
	}
	if result.Updated || result.RestartPending {
		t.Errorf("已是最新时不该有落地动作，实际 %+v", result)
	}
	if spawned {
		t.Error("已是最新时不该启动收尾助手")
	}
	if !strings.Contains(result.Message, "最新") {
		t.Errorf("提示应说明已是最新，实际 %q", result.Message)
	}
}

// TestPerformSpawnsHelperWithStopFlagForCLI 锁定 CLI 路径的助手命令行：
// 必须带上"主动停旧"的开关（发起更新的不是那个在服务的进程）。
func TestPerformSpawnsHelperWithStopFlagForCLI(t *testing.T) {
	const version = "9.9.9"
	asset := AssetName(version, runtime.GOOS, runtime.GOARCH)
	withFakeReleases(t, asset, []byte("新版本内容"))
	dir, executable := newInstalledBinary(t, "旧版本内容")

	var gotCommand []string
	var gotLogPath string
	result, err := Perform(Options{
		Check:      stubCheck("5.1.1", version),
		Executable: executable,
		ConfigPath: filepath.Join(dir, "router-config.json"),
		StopOld:    true,
		Spawn: func(command []string, logPath string) (int, error) {
			gotCommand, gotLogPath = command, logPath
			return 4242, nil
		},
	})
	if err != nil {
		t.Fatalf("Perform 报错: %v", err)
	}
	if !result.Updated || !result.RestartPending {
		t.Errorf("应报告已更新且待重启，实际 %+v", result)
	}
	if gotCommand[len(gotCommand)-1] != HelperStopFlag {
		t.Errorf("CLI 路径的助手命令应以 %s 结尾，实际 %v", HelperStopFlag, gotCommand)
	}
	if !strings.Contains(strings.Join(gotCommand, " "), HelperFlag) {
		t.Errorf("命令里应含 %s，实际 %v", HelperFlag, gotCommand)
	}
	// 助手指向**目标**可执行文件，日志落在它旁边。
	if filepath.Base(gotCommand[0]) != filepath.Base(executable) {
		t.Errorf("助手应指向目标可执行文件，实际 %q", gotCommand[0])
	}
	if filepath.Dir(gotLogPath) != filepath.Dir(executable) {
		t.Errorf("日志应在可执行文件目录下，实际 %q", gotLogPath)
	}
	// 配置文件也要传给助手：它得按同一份配置判断注册形态。
	if !strings.Contains(strings.Join(gotCommand, " "), "router-config.json") {
		t.Errorf("命令应带配置文件，实际 %v", gotCommand)
	}

	got, readErr := os.ReadFile(executable)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "新版本内容" {
		t.Errorf("安装后内容 = %q", got)
	}
}

// TestPerformOmitsStopFlagOnServerPath 锁定服务端路径不带停旧开关。
//
// 这条有实测依据：服务端发起更新时，发起者就是那个服务进程，它自己会优雅退出；若让
// 助手去"停"，后台服务形态走 `taskkill /T /F`，实测会把助手自己一起杀掉。
func TestPerformOmitsStopFlagOnServerPath(t *testing.T) {
	const version = "9.9.9"
	asset := AssetName(version, runtime.GOOS, runtime.GOARCH)
	withFakeReleases(t, asset, []byte("新版本内容"))
	_, executable := newInstalledBinary(t, "旧版本内容")

	var gotCommand []string
	if _, err := Perform(Options{
		Check:      stubCheck("5.1.1", version),
		Executable: executable,
		StopOld:    false,
		Spawn: func(command []string, _ string) (int, error) {
			gotCommand = command
			return 1, nil
		},
	}); err != nil {
		t.Fatalf("Perform 报错: %v", err)
	}
	for _, arg := range gotCommand {
		if arg == HelperStopFlag {
			t.Errorf("服务端路径不该带 %s：%v", HelperStopFlag, gotCommand)
		}
	}
}

// TestPerformSurvivesHelperLaunchFailure 锁定"助手没起来不算更新失败"：
// 文件已经换好，只是需要手动重启。
func TestPerformSurvivesHelperLaunchFailure(t *testing.T) {
	const version = "9.9.9"
	asset := AssetName(version, runtime.GOOS, runtime.GOARCH)
	withFakeReleases(t, asset, []byte("新版本内容"))
	_, executable := newInstalledBinary(t, "旧版本内容")

	result, err := Perform(Options{
		Check:      stubCheck("5.1.1", version),
		Executable: executable,
		StopOld:    true,
		Spawn: func([]string, string) (int, error) {
			return 0, os.ErrPermission
		},
	})
	if err != nil {
		t.Fatalf("文件已换好时不该报错，实际 %v", err)
	}
	if !result.Updated {
		t.Error("应报告已更新（文件确实换了）")
	}
	if result.RestartPending {
		t.Error("助手没起来时不该报告待重启")
	}
	if !strings.Contains(result.Message, "手动重启") {
		t.Errorf("应提示手动重启，实际 %q", result.Message)
	}
	got, readErr := os.ReadFile(executable)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "新版本内容" {
		t.Errorf("安装后内容 = %q", got)
	}
}

// TestPerformReportsCheckFailure 锁定"检查失败 → 报错且什么都不做"。
func TestPerformReportsCheckFailure(t *testing.T) {
	reason := "连不上 GitHub"
	_, executable := newInstalledBinary(t, "原版")
	spawned := false

	result, err := Perform(Options{
		Check: func() updatecheck.Result {
			return updatecheck.Result{CurrentVersion: "5.1.1", Error: &reason}
		},
		Executable: executable,
		Spawn: func([]string, string) (int, error) {
			spawned = true
			return 1, nil
		},
	})
	if err == nil {
		t.Fatal("检查失败时 Perform 必须报错")
	}
	if !strings.Contains(err.Error(), reason) {
		t.Errorf("错误应包含原因，实际 %v", err)
	}
	if result.Updated || spawned {
		t.Error("检查失败时不该有任何落地动作")
	}
	got, _ := os.ReadFile(executable)
	if string(got) != "原版" {
		t.Error("检查失败时不该改动已安装的二进制")
	}
}

// TestPerformRefusesInstallWhenChecksumMissing 锁定"校验和里没有这个产物 → 拒绝安装"。
//
// 这是一条安全边界：拿不到校验和时绝不能"跳过校验直接装"。
func TestPerformRefusesInstallWhenChecksumMissing(t *testing.T) {
	const version = "9.9.9"

	// 假服务只提供产物，checksums.txt 里写的是**别的**文件名。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/checksums.txt") {
			_, _ = w.Write([]byte(strings.Repeat("a", 64) + "  amkr_9.9.9_别的平台\n"))
			return
		}
		_, _ = w.Write([]byte("新版本内容"))
	}))
	defer server.Close()
	defer withReleasesBaseURL(server.URL)()

	_, executable := newInstalledBinary(t, "原版")
	result, err := Perform(Options{
		Check:      stubCheck("5.1.1", version),
		Executable: executable,
	})
	if err == nil {
		t.Fatal("校验和缺失时必须报错")
	}
	if result.Updated {
		t.Error("不该报告已更新")
	}
	got, _ := os.ReadFile(executable)
	if string(got) != "原版" {
		t.Errorf("已安装的二进制被改动了: %q", got)
	}
}

// TestPerformWithoutSpawnerOnlySwapsFile 锁定 Spawn 为 nil 时只换文件（CLI 在无权重启
// 的那条路上就是这么调的）。
func TestPerformWithoutSpawnerOnlySwapsFile(t *testing.T) {
	const version = "9.9.9"
	asset := AssetName(version, runtime.GOOS, runtime.GOARCH)
	withFakeReleases(t, asset, []byte("新版本内容"))
	_, executable := newInstalledBinary(t, "旧版本内容")

	result, err := Perform(Options{
		Check:      stubCheck("5.1.1", version),
		Executable: executable,
		Spawn:      nil,
	})
	if err != nil {
		t.Fatalf("Perform 报错: %v", err)
	}
	if !result.Updated || result.RestartPending {
		t.Errorf("应报告已更新但不待重启，实际 %+v", result)
	}
	if !strings.Contains(result.Message, "重启") {
		t.Errorf("应提示需要重启，实际 %q", result.Message)
	}
}
