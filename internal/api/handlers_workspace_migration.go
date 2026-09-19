package api

import (
	"net/http"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/config"
	"github.com/Sparrived/auto-model-key-router/internal/configops"
)

// 工作空间的**整包迁移**端点：把一个或多个空间连任务带面板 key 一起导出、导入。
//
// 为什么与 /api/config/export 分成两条路（而不是给它加个参数）：那条通道的语义是
// 「整台实例的供应商+模型搬走」，它**刻意剥掉 api_key**（导出文件会被贴进工单与聊天
// 记录），并且合并时按 base_url / key secret 去重、按模型 id 合并。对一个只想把自己
// 的空间搬到另一个实例、并保留面板凭据的应用来说，这两条都不对。详见
// configops/workspace_bundle.go 的文件头。
//
// 两条路都只认完整权限：这里有明文面板 key，比配置导出更敏感。

// —— POST /api/workspaces/export ——

// handleExportWorkspaces 导出工作空间迁移包（带 api_key）。
//
// body 里的 workspaces 为空表示导出全部命名工作空间。默认工作空间不在其中：它没有
// 名字也没有 key，搬过去等于覆盖对方的默认空间。
func (s *Server) handleExportWorkspaces(w http.ResponseWriter, r *http.Request) {
	s.run(w, http.StatusOK, func() (*canonical.Value, error) {
		if _, err := s.authorizedConfig(r); err != nil {
			return nil, err
		}
		payload, present, err := decodePayload(r, specWorkspaceExport, true)
		if err != nil {
			return nil, err
		}
		var names []string
		if present {
			names = trailingStringSlice(payload, "workspaces")
		}
		data, err := s.managementConfigData()
		if err != nil {
			return nil, err
		}
		bundle, err := configops.ExportWorkspaces(data, names)
		if err != nil {
			// 这条路径不经过 _update_config，因此 configops 的 ConfigOperationError
			// 不会自动翻译成它的 status_code（顶层冒泡会变成 500，那是给「更新配置
			// 时炸了」准备的兜底）。显式过一次同一套翻译，"空间不存在"才是 404。
			return nil, translateUpdateError(err)
		}
		return withRevision(data, objectOf(
			canonical.ObjectPair{Key: "bundle", Value: configops.WorkspaceBundleValue(bundle)},
		))
	})
}

// —— POST /api/workspaces/import ——

// handleImportWorkspaces 把一份工作空间迁移包并入当前配置。
//
// 与配置导入一致：先备份当前配置再改，且整体走 _update_config（写锁 + revision 比对
// + 原子落盘）。备份是必须的——导入会换掉同名空间的面板 key，旧 key 立刻失效，没有
// 备份就没有回头路。
func (s *Server) handleImportWorkspaces(w http.ResponseWriter, r *http.Request) {
	s.run(w, http.StatusOK, func() (*canonical.Value, error) {
		payload, _, err := decodePayload(r, specWorkspaceImport, false)
		if err != nil {
			return nil, err
		}
		bundle, err := configops.ParseWorkspaceBundle(payload.Lookup("bundle"))
		if err != nil {
			// 同上：这条路径在 _update_config 之外，op 错误要显式翻译，否则畸形的包
			// 会变成 500「Internal Server Error」，而它其实是调用方的输入问题。
			return nil, translateUpdateError(err)
		}
		prefix := ""
		if given := optString(payload, "prefix"); given != nil {
			prefix = *given
		}

		var result configops.WorkspaceImportResult
		if _, err := s.updateConfig(r, func(current *canonical.Value) error {
			backupPath := importBackupPath(s.ConfigPath, s.uuidHex())
			if err := config.SaveConfigData(backupPath, current); err != nil {
				return err
			}
			var importErr error
			result, importErr = configops.ImportWorkspaces(current, bundle, prefix)
			return importErr
		}, optString(payload, "config_revision")); err != nil {
			return nil, err
		}
		data, err := s.managementConfigData()
		if err != nil {
			return nil, err
		}
		renamed := canonical.NewObject()
		for from, to := range result.Renamed {
			renamed.SetKey(from, canonical.NewString(to))
		}
		// 只有加前缀克隆才会换 key（原空间还在，占着原 key）。返回新 key 是必须的：
		// 嵌入方手里那把对应的是**原**空间，克隆出来的空间没有可用的面板。
		rekeyed := canonical.NewObject()
		for name, key := range result.Rekeyed {
			rekeyed.SetKey(name, canonical.NewString(key))
		}
		return withRevision(data, objectOf(
			canonical.ObjectPair{Key: "imported", Value: canonical.NewBool(true)},
			canonical.ObjectPair{Key: "added", Value: stringArray(result.Added)},
			canonical.ObjectPair{Key: "replaced", Value: stringArray(result.Replaced)},
			canonical.ObjectPair{Key: "renamed", Value: renamed},
			canonical.ObjectPair{Key: "rekeyed", Value: rekeyed},
			canonical.ObjectPair{Key: "removed_tasks", Value: stringArray(result.RemovedTasks)},
		))
	})
}
