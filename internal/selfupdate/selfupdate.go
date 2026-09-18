// Package selfupdate 实现从 GitHub Releases 自更新：挑选产物、下载、校验、就地替换。
//
// 为什么 Go 版可以做得比 Python 版小得多：Python 的 update.py 有约 573 行，绝大部分
// 在处理「运行中的可执行文件被锁住」。Windows 其实允许**重命名**正在运行的 exe
// （只是不允许覆盖或删除它），因此可以先把旧的改名挪开、再把新的放到原位——旧进程
// 继续服务到重启为止。整个替换就是两次 os.Rename，没有平台分支。
//
// 本包只做**纯逻辑与文件操作**，不碰服务重启：那需要 internal/service 的平台知识，
// 由调用方（cmd/amkr）编排。这样下载、校验与替换都能脱离进程管理单独测试。
package selfupdate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Sparrived/auto-model-key-router/internal/updatecheck"
)

// ReleasesBaseURL 是发布物的下载根地址（不含标签）。
//
// 是变量而不是常量：测试要把它指向 httptest 假服务，否则测一次成功路径就得真的去
// GitHub 下载十几兆。生产代码从不改写它。
var releasesBaseURL = "https://github.com/" + updatecheck.GitHubRepository + "/releases/download"

// ReleasesBaseURL 返回当前生效的下载根地址。
func ReleasesBaseURL() string { return releasesBaseURL }

// AssetName 返回某平台的产物文件名，与 release.yml:125 的命名逐字一致：
//
//	amkr_<version>_<goos>_<goarch>[.exe]
//
// 两边必须一致，否则自更新会去下一个不存在的文件（表现为 404）。该断言由测试锁定。
func AssetName(version, goos, goarch string) string {
	name := fmt.Sprintf("amkr_%s_%s_%s", version, goos, goarch)
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// DownloadURL 返回产物地址；ChecksumsURL 返回同标签下的校验和文件地址。
func DownloadURL(version, asset string) string {
	return ReleasesBaseURL() + "/v" + version + "/" + asset
}

func ChecksumsURL(version string) string {
	return ReleasesBaseURL() + "/v" + version + "/checksums.txt"
}

// ParseChecksums 从 checksums.txt 里取出某个文件的 sha256。
//
// 行格式与 `sha256sum` 一致：`<hash>  <name>`（二进制模式为 `<hash> *<name>`）。
// 找不到返回 false——调用方必须据此**拒绝安装**，绝不能跳过校验。
func ParseChecksums(text, asset string) (string, bool) {
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") == asset {
			return strings.ToLower(fields[0]), true
		}
	}
	return "", false
}

// VerifySHA256 校验文件的 sha256，不匹配返回错误。
func VerifySHA256(path, expected string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	actual := hex.EncodeToString(hash.Sum(nil))
	if expected = strings.ToLower(expected); actual != expected {
		return fmt.Errorf("sha256 校验失败：期望 %s，实际 %s", expected, actual)
	}
	return nil
}

// Download 把 url 取回并写入 path；client 为 nil 时用 http.DefaultClient。
func Download(client *http.Client, url, path string) error {
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Get(url)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", response.StatusCode, url)
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := io.Copy(file, response.Body); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

// FetchChecksums 取回并解析给定版本的校验和文件。
func FetchChecksums(client *http.Client, version, asset string) (string, error) {
	url := ChecksumsURL(version)
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %d: %s", response.StatusCode, url)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return "", err
	}
	digest, ok := ParseChecksums(string(body), asset)
	if !ok {
		return "", fmt.Errorf("checksums.txt 里没有 %s", asset)
	}
	return digest, nil
}

// StaleSuffix 是替换时旧二进制被挪去的后缀。
//
// 旧文件在**旧进程退出前无法删除**（Windows 不允许删除正在运行的映像），只能改名。
// 因此替换后必然短暂留下一个 <exe>.old，由收尾的助手进程在旧进程退出后清掉。
// 这就是「不留备份」的落地方式：该文件只是替换过程的中间态，不是可回退的备份。
const StaleSuffix = ".old"

// StalePath 返回 target 对应的旧文件暂存路径。
func StalePath(target string) string { return target + StaleSuffix }

// exeName 是暂存文件的名字。
//
// **必须**以 .exe 结尾（Windows）：否则连 `amkr-new --version` 都跑不起来，
// 报「无法在管道中间运行文档」。这一点是实测出来的，不要改成 .new。
func exeName() string {
	if runtime.GOOS == "windows" {
		return "amkr-new.exe"
	}
	return "amkr-new"
}

// Stage 把已下载的二进制复制到目标目录的暂存位置，返回暂存路径。
//
// 必须先落到目标目录再改名：下载用的临时目录可能与安装目录不同卷，跨卷 rename
// 会失败（且退化成整文件复制）。
func Stage(downloaded, targetDir string) (string, error) {
	staged := filepath.Join(targetDir, exeName())
	source, err := os.Open(downloaded)
	if err != nil {
		return "", err
	}
	defer func() { _ = source.Close() }()
	destination, err := os.OpenFile(staged, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(destination, source); err != nil {
		_ = destination.Close()
		_ = os.Remove(staged)
		return "", err
	}
	if err := destination.Close(); err != nil {
		_ = os.Remove(staged)
		return "", err
	}
	return staged, nil
}

// Replace 就地替换 target，返回被挪开的旧文件路径（供旧进程退出后删除）。
//
// 顺序不可交换：先把旧的改名让位，再把新的放到原位。反过来会因目标已存在而失败。
// 第二步失败时会把旧文件改回来，避免把用户留在「没有可执行文件」的状态。
func Replace(target, staged string) (string, error) {
	stale := StalePath(target)
	// 上一次更新可能留下未清理的 .old（例如助手被连坐杀掉）。尽力清掉；清不掉不阻断，
	// 下面的 rename 会覆盖它。
	_ = os.Remove(stale)
	if err := os.Rename(target, stale); err != nil {
		return "", fmt.Errorf("挪开旧版本失败: %w", err)
	}
	if err := os.Rename(staged, target); err != nil {
		_ = os.Rename(stale, target)
		return "", fmt.Errorf("放入新版本失败: %w", err)
	}
	return stale, nil
}
