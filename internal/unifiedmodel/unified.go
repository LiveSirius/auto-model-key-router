// Package unifiedmodel 提供 unified 模型目标的便捷切换入口。
//
// 移植 auto_model_key_router/unified_model.py（65 行）。它本身不含业务逻辑，只是把
// configops.SwitchUnifiedTarget 与 configservice.ConfigService.Update 组合起来：
// 走同一条「读-改-写」路径，因此与其它配置修改共享同一把按路径的锁，不会互相覆盖。
//
// 单独成包而不是塞进 configservice：configservice 不该依赖 configops（那是更高层的
// 配置操作），否则将来 configops 若需要 configservice 就会形成导入环。
package unifiedmodel

import (
	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/config"
	"github.com/Sparrived/auto-model-key-router/internal/configops"
	"github.com/Sparrived/auto-model-key-router/internal/configservice"
)

// switchUnifiedTarget 落盘单个 unified 目标。
//
// target 形如 "default.primary" / "image.fallback"，取值集合见
// configops.UnifiedTargets；非法目标返回 422。
func switchUnifiedTarget(configPath, target string, modelName, keyName *string, updateKey bool) (*config.RouterConfig, error) {
	change, err := configservice.New(configPath).Update(func(data *canonical.Value) error {
		return configops.SwitchUnifiedTarget(data, target, modelName, keyName, updateKey)
	})
	if err != nil {
		return nil, err
	}
	return change.NewConfig, nil
}

// SwitchUnifiedTarget 落盘一个 unified 目标并返回变更后的配置。
//
// 对应 unified_model.py:10 的 switch_unified_target。configPath 为 nil 之外的任意
// 路径字符串；传空串时由 configservice 按未解析路径处理。
func SwitchUnifiedTarget(configPath, target string, modelName, keyName *string, updateKey bool) (*config.RouterConfig, error) {
	return switchUnifiedTarget(configPath, target, modelName, keyName, updateKey)
}

// SwitchUnifiedModel 是 default / image 两个主槽位的兼容入口。
//
// 对应 unified_model.py:34 的 switch_unified_model。两个要点：
//
//  1. 两次 apply_unified_target 在**同一次 mutation** 内完成，因此是**一次原子写盘**
//     （参照实现同样只调一次 ConfigService.update）。分成两次提交会出现「只改了 default
//     而 image 失败」的半成品状态。
//  2. image 槽位**仅当** imageModelName 或 imageKeyName 至少一个非 nil 时才动
//     （unified_model.py:56 的 `is not None` 判断）。注意这里判的是 nil 而不是空串：
//     显式传空串会真的把 image 主槽位置空，传 nil 才表示"不改 image"。
func SwitchUnifiedModel(
	configPath string,
	modelName, keyName *string,
	updateKey bool,
	imageModelName, imageKeyName *string,
	updateImageKey bool,
) (*config.RouterConfig, error) {
	change, err := configservice.New(configPath).Update(func(data *canonical.Value) error {
		if err := configops.SwitchUnifiedTarget(data, "default.primary", modelName, keyName, updateKey); err != nil {
			return err
		}
		if imageModelName != nil || imageKeyName != nil {
			return configops.SwitchUnifiedTarget(data, "image.primary", imageModelName, imageKeyName, updateImageKey)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return change.NewConfig, nil
}
