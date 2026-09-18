#!/bin/sh
# AMKR 安装脚本（Linux / macOS）。
#
# 用法：
#   curl -fsSL https://raw.githubusercontent.com/Sparrived/auto-model-key-router/master/scripts/install.sh | sh
#   sh install.sh --version 5.0.0
#   INSTALL_DIR="$HOME/bin" sh install.sh
#
# 只依赖 POSIX sh、curl/wget 与 sha256sum/shasum（macOS 用 shasum）：下载对应平台的
# 发布物，用发布页上的 checksums.txt 校验 sha256，装到可写的 bin 目录并打印结果。
# 校验不通过会立刻退出——绝不安装校验不过的文件。
#
# Windows 请改用 scripts/install.ps1（PowerShell）。

set -eu

REPO="Sparrived/auto-model-key-router"
PROGRAM="amkr"
API_LATEST="https://api.github.com/repos/${REPO}/releases/latest"
RELEASE_BASE="https://github.com/${REPO}/releases/download"

VERSION=""
INSTALL_DIR="${INSTALL_DIR:-}"

say() { printf '%s\n' "$*"; }
die() { printf 'amkr 安装失败: %s\n' "$*" >&2; exit 1; }

usage() {
	cat <<'EOF'
用法: install.sh [--version X.Y.Z] [--help]

  --version X.Y.Z   安装指定版本（默认取 GitHub 上最新的 release）
  -h, --help        显示本帮助

环境变量:
  INSTALL_DIR       安装目录（默认 /usr/local/bin；不可写时退回 ~/.local/bin）

示例:
  sh install.sh --version 5.0.0
  INSTALL_DIR="$HOME/.local/bin" sh install.sh
EOF
}

while [ "$#" -gt 0 ]; do
	case "$1" in
	--version)
		[ "$#" -ge 2 ] || die "--version 需要版本号，例如 --version 5.0.0"
		VERSION="$2"
		shift 2
		;;
	--version=*)
		VERSION="${1#--version=}"
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		die "未知参数: $1（--help 查看用法）"
		;;
	esac
done

# 允许 --version v5.0.0 这种带 v 的写法。
VERSION="${VERSION#v}"

# —— 下载 —— #

fetch_file() { # url dest
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$1" -o "$2"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$2" "$1"
	else
		die "需要 curl 或 wget 才能下载"
	fi
}

fetch_stdout() { # url
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O - "$1"
	else
		die "需要 curl 或 wget 才能下载"
	fi
}

# —— 平台识别 —— #

detect_os() {
	case "$(uname -s)" in
	Linux) printf 'linux\n' ;;
	Darwin) printf 'darwin\n' ;;
	MINGW* | MSYS* | CYGWIN*)
		die "当前是 Windows 环境（$(uname -s)）：请改用 PowerShell 脚本 scripts/install.ps1"
		;;
	*)
		die "不支持的系统: $(uname -s)（发布物只有 linux 与 darwin）"
		;;
	esac
}

detect_arch() {
	case "$(uname -m)" in
	x86_64 | amd64) printf 'amd64\n' ;;
	arm64 | aarch64) printf 'arm64\n' ;;
	*)
		die "不支持的 CPU 架构: $(uname -m)（发布物只有 amd64 与 arm64）"
		;;
	esac
}

# —— 版本与校验和 —— #

resolve_version() {
	say "未指定版本，查询 GitHub 上的最新 release..."
	tag="$(fetch_stdout "$API_LATEST" | tr ',' '\n' | grep '"tag_name"' | head -n 1 || true)"
	latest="$(printf '%s' "$tag" | sed -e 's/.*"tag_name"[^"]*"//' -e 's/".*//')"
	latest="${latest#v}"
	[ -n "$latest" ] || die "无法解析最新版本，请显式指定 --version X.Y.Z"
	VERSION="$latest"
}

sha256_of() { # file
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | cut -d' ' -f1
	else
		die "需要 sha256sum 或 shasum 才能校验下载物"
	fi
}

expected_sha() { # checksums_file asset
	# 行格式与 `sha256sum` 一致：<hash>  <name>（二进制模式写成 <hash> *<name>）。
	# 末尾的 \r 也要去掉：在 Windows 上手工生成的 checksums.txt 会带 CRLF。
	awk -v want="$2" '{
		name = $2
		sub(/^\*/, "", name)
		sub(/\r$/, "", name)
		if (name == want) { print $1; exit }
	}' "$1"
}

# —— 安装目录 —— #

choose_install_dir() {
	if [ -n "$INSTALL_DIR" ]; then
		printf '%s\n' "$INSTALL_DIR"
		return 0
	fi
	if [ -d /usr/local/bin ] && [ -w /usr/local/bin ]; then
		printf '/usr/local/bin\n'
		return 0
	fi
	printf '%s\n' "$HOME/.local/bin"
}

# —— 主流程 —— #

OS="$(detect_os)"
ARCH="$(detect_arch)"
[ -n "$VERSION" ] || resolve_version

ASSET="${PROGRAM}_${VERSION}_${OS}_${ARCH}"
ASSET_URL="${RELEASE_BASE}/v${VERSION}/${ASSET}"
CHECKSUMS_URL="${RELEASE_BASE}/v${VERSION}/checksums.txt"

WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/amkr-install.XXXXXX")"
trap 'rm -rf "$WORKDIR"' EXIT INT TERM HUP

say "下载 ${ASSET}（v${VERSION}，${OS}/${ARCH}）..."
fetch_file "$ASSET_URL" "${WORKDIR}/${ASSET}" ||
	die "下载失败: ${ASSET_URL}（该版本或平台可能没有发布物）"
fetch_file "$CHECKSUMS_URL" "${WORKDIR}/checksums.txt" ||
	die "下载校验和失败: ${CHECKSUMS_URL}"

EXPECTED="$(expected_sha "${WORKDIR}/checksums.txt" "$ASSET")"
[ -n "$EXPECTED" ] || die "checksums.txt 里没有 ${ASSET} 的记录，拒绝安装"
ACTUAL="$(sha256_of "${WORKDIR}/${ASSET}")"
[ "$ACTUAL" = "$EXPECTED" ] ||
	die "sha256 校验失败：${ASSET} 期望 ${EXPECTED}，实际 ${ACTUAL}（下载可能被篡改或损坏）"
say "sha256 校验通过: ${ACTUAL}"

TARGET="$(choose_install_dir)"
mkdir -p "$TARGET" || die "创建安装目录失败: $TARGET"
INSTALLED="${TARGET}/${PROGRAM}"
cp "${WORKDIR}/${ASSET}" "$INSTALLED" ||
	die "写入 ${INSTALLED} 失败（若 amkr 正在运行，先执行 amkr --stop 再重试）"
chmod 755 "$INSTALLED"

say "已安装: ${INSTALLED}（v${VERSION}）"
if VERSION_OUTPUT="$("$INSTALLED" --version 2>/dev/null)"; then
	say "自检: ${VERSION_OUTPUT}"
else
	say "提示: 无法执行 ${INSTALLED} --version，请手动确认该二进制能在本机运行。"
fi

case ":${PATH}:" in
*":${TARGET}:"*) ;;
*)
	say "提示: ${TARGET} 不在 PATH 里，把它加进 shell 配置后重开终端："
	say "  export PATH=\"${TARGET}:\$PATH\""
	;;
esac
