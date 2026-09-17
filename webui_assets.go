// Package amkr 是模块根包，唯一职责是**把随仓库发布的 WebUI 静态资产编译进二进制**。
//
// 为什么需要它：`//go:embed` 的模式不能引用父目录（"patterns must not contain '..'"），
// 而资产在 auto_model_key_router/webui 下。internal/server 因此把资产做成注入参数
// （server.Options.WebUIAssets），由 cmd/amkr 绑定这里暴露的 FS。资产目录与 Go 文件
// 同级的唯一做法就是把 Go 文件放在仓库根，这正是本文件的位置。
//
// 代价与取舍：Go 构建从此依赖 auto_model_key_router/webui 下的文件存在（它们同时是
// Python 版的发布物）。Python 退役后如果资产搬到别处，只需改这里的模式字符串。
package amkr

import (
	"embed"
	"io/fs"
)

// assetsDir 是 WebUI 资产在仓库里的相对路径（webui.py:24 的 _ASSET_DIR 对应物）。
const assetsDir = "auto_model_key_router/webui"

//go:embed auto_model_key_router/webui
var embeddedWebUI embed.FS

// WebUIAssets 是**以 WebUI 资产目录为根**的只读文件系统。
//
// 根必须是资产目录本身：internal/webui 的处理器把请求路径当资产根内的相对名解析
// （见 internal/webui/webui.go 的 resolveName）。
var WebUIAssets = mustSub(embeddedWebUI, assetsDir)

// mustSub 取出子目录；模式与目录都是编译期常量，取不到说明 embed 指令被改错了，
// 属于程序员错误而不是运行时状况，因此直接 panic。
func mustSub(source fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(source, dir)
	if err != nil {
		panic("amkr: 嵌入的 WebUI 资产目录不可用: " + err.Error())
	}
	return sub
}
