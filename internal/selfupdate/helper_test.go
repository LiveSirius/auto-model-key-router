package selfupdate

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 本文件覆盖收尾助手：动作顺序、以及两条"什么都不该做"的路径。
//
// 助手全是注入的钩子，因此可以在毫秒级把「等 30 秒」压缩成几次循环，且完全不碰真实
// 服务与真实进程。

// recordingHooks 记录调用顺序，并让「已停止」在第 N 次询问后成立（模拟旧进程退出）。
type recordingHooks struct {
	calls    []string
	stopErr  error
	startErr error
	// stoppedAfter 表示第几次询问 Stopped 时返回真；0 表示立刻就是停的。
	stoppedAfter int
	asked        int
}

func (h *recordingHooks) hooks(running bool, stale string) HelperHooks {
	return HelperHooks{
		Running: running,
		Stop: func() error {
			h.calls = append(h.calls, "stop")
			return h.stopErr
		},
		Stopped: func() bool {
			h.asked++
			return h.asked > h.stoppedAfter
		},
		Sleep: func(time.Duration) { h.calls = append(h.calls, "sleep") },
		Start: func() error {
			h.calls = append(h.calls, "start")
			return h.startErr
		},
	}
}

// TestRunHelperFullSequence 锁定顺序：停 → 等停 → 删旧 → 启动。
//
// 顺序不可交换：先删 .old 会因映像仍在被映射而失败；先启动新版本会因端口被占而失败。
func TestRunHelperFullSequence(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "amkr.exe.old")
	if err := os.WriteFile(stale, []byte("旧"), 0o644); err != nil {
		t.Fatal(err)
	}
	recorder := &recordingHooks{stoppedAfter: 2}
	var log bytes.Buffer

	RunHelper(HelperOptions{Stale: stale, StopOld: true, Log: &log},
		func() HelperHooks {
			hooks := recorder.hooks(true, stale)
			// 把"删文件"也记进顺序里，才能断言它落在停止之后、启动之前。
			return hooks
		}())

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("旧文件应当已被清理")
	}
	got := strings.Join(recorder.calls, ",")
	// 两次 sleep 之后才"停止"，然后是 start。
	if got != "stop,sleep,sleep,start" {
		t.Errorf("调用顺序 = %s，期望 stop,sleep,sleep,start", got)
	}
	if !strings.Contains(log.String(), "旧服务已停止") {
		t.Errorf("日志应记录已停止，实际 %q", log.String())
	}
}

// TestRunHelperCleansStaleBeforeStart 用文件存在性证明"删除发生在启动之前"：
// 在 Start 钩子里检查 .old 是否已经没了。
func TestRunHelperCleansStaleBeforeStart(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "amkr.exe.old")
	if err := os.WriteFile(stale, []byte("旧"), 0o644); err != nil {
		t.Fatal(err)
	}
	staleGoneAtStart := false
	hooks := HelperHooks{
		Running: true,
		Stop:    func() error { return nil },
		Stopped: func() bool { return true },
		Sleep:   func(time.Duration) {},
		Start: func() error {
			_, err := os.Stat(stale)
			staleGoneAtStart = os.IsNotExist(err)
			return nil
		},
	}

	RunHelper(HelperOptions{Stale: stale, StopOld: true}, hooks)

	if !staleGoneAtStart {
		t.Error("启动新版本时旧文件应当已经清理掉")
	}
}

// TestRunHelperSkipsEverythingWhenNothingWasRunning 锁定"更新前没实例在跑"时
// **不重启**：用户只是跑了一次 `amkr --update`，不该因此凭空多出一个服务。
func TestRunHelperSkipsEverythingWhenNothingWasRunning(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "amkr.exe.old")
	if err := os.WriteFile(stale, []byte("旧"), 0o644); err != nil {
		t.Fatal(err)
	}
	recorder := &recordingHooks{}
	var log bytes.Buffer

	RunHelper(HelperOptions{Stale: stale, StopOld: true, Log: &log}, recorder.hooks(false, stale))

	if len(recorder.calls) != 0 {
		t.Errorf("不该有任何停止/启动调用，实际 %v", recorder.calls)
	}
	// 但旧文件仍要清理：它是一次性的替换中间态。
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("即使不重启也应清理旧文件")
	}
	if !strings.Contains(log.String(), "跳过停止与重启") {
		t.Errorf("日志应说明跳过，实际 %q", log.String())
	}
}

// TestRunHelperSkipsStopButStillStartsOnServerPath 锁定服务端路径（StopOld=false）：
// 不主动停（发起更新的进程自己会退出），但要等端口释放、清理、重启。
func TestRunHelperSkipsStopButStillStartsOnServerPath(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "amkr.exe.old")
	if err := os.WriteFile(stale, []byte("旧"), 0o644); err != nil {
		t.Fatal(err)
	}
	recorder := &recordingHooks{stoppedAfter: 1}

	RunHelper(HelperOptions{Stale: stale, StopOld: false},
		recorder.hooks(true, stale))

	got := strings.Join(recorder.calls, ",")
	if strings.Contains(got, "stop") {
		t.Errorf("服务端路径不该调用 Stop（%s）：发起更新的进程自己会退出", got)
	}
	if got != "sleep,start" {
		t.Errorf("调用顺序 = %s，期望 sleep,start", got)
	}
}

// TestRunHelperGivesUpWhenServiceNeverStops 锁定超时路径：
// 旧服务始终不让出端口时，不启动新版本（启动了也会绑定失败）。
func TestRunHelperGivesUpWhenServiceNeverStops(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "amkr.exe.old")
	if err := os.WriteFile(stale, []byte("旧"), 0o644); err != nil {
		t.Fatal(err)
	}
	recorder := &recordingHooks{stoppedAfter: 1 << 30} // 永远不"已停止"
	var log bytes.Buffer

	RunHelper(HelperOptions{
		Stale:   stale,
		StopOld: true,
		Timeout: 5 * time.Millisecond,
		Log:     &log,
	}, recorder.hooks(true, stale))

	if strings.Contains(strings.Join(recorder.calls, ","), "start") {
		t.Error("超时后不该尝试启动（端口仍被占用，必然失败）")
	}
	// 旧文件也不能删：映像还映射着它，删除会失败。留给下次启动时清理。
	if _, err := os.Stat(stale); err != nil {
		t.Errorf("超时时应保留旧文件，实际 %v", err)
	}
	if !strings.Contains(log.String(), "跳过收尾") {
		t.Errorf("日志应说明跳过收尾，实际 %q", log.String())
	}
}

// TestRunHelperLogsButSurvivesStopFailure 锁定"停失败只记日志，不中断收尾"。
func TestRunHelperLogsButSurvivesStopFailure(t *testing.T) {
	recorder := &recordingHooks{stopErr: errors.New("拒绝访问")}
	var log bytes.Buffer

	RunHelper(HelperOptions{StopOld: true, Log: &log}, recorder.hooks(true, ""))

	if !strings.Contains(log.String(), "拒绝访问") {
		t.Errorf("应记录停止失败的原因，实际 %q", log.String())
	}
	if !strings.Contains(strings.Join(recorder.calls, ","), "start") {
		t.Error("停止失败后仍应尝试收尾（旧进程也许本来就退出了）")
	}
}

// TestRunHelperLogsStartFailure 锁定"启动失败只记日志"：文件已经换好了，
// 不该让用户以为更新失败。
func TestRunHelperLogsStartFailure(t *testing.T) {
	recorder := &recordingHooks{startErr: errors.New("绑定失败")}
	var log bytes.Buffer

	RunHelper(HelperOptions{StopOld: true, Log: &log}, recorder.hooks(true, ""))

	if !strings.Contains(log.String(), "绑定失败") {
		t.Errorf("应记录启动失败原因，实际 %q", log.String())
	}
}

// TestRunHelperWorksWithoutLog 锁定 Log 为 nil 时不 panic（默认零值最容易漏）。
func TestRunHelperWorksWithoutLog(t *testing.T) {
	recorder := &recordingHooks{}
	RunHelper(HelperOptions{StopOld: true}, recorder.hooks(true, ""))
	if !strings.Contains(strings.Join(recorder.calls, ","), "start") {
		t.Error("没有日志输出时也应正常收尾")
	}
}

// TestCleanStale 锁定残留清理（新版本启动时清掉上次没删掉的 .old）。
func TestCleanStale(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "amkr.exe")

	// 没有 .old 时返回 false（表示没清任何东西）。
	if CleanStale(executable) {
		t.Error("没有残留时 CleanStale 应返回 false")
	}
	stale := StalePath(executable)
	if err := os.WriteFile(stale, []byte("旧"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !CleanStale(executable) {
		t.Error("有残留时 CleanStale 应返回 true")
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("残留应已被删除")
	}
}
