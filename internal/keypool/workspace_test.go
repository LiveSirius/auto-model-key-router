package keypool

import (
	"testing"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
	"github.com/Sparrived/auto-model-key-router/internal/config"
)

// 本文件固化 keypool 里工作空间的查表语义。
//
// 工作空间是 Go 侧新增能力（参照实现没有），因此这些用例是手写的。关键点是
// taskPlans / taskParams 用 (工作空间, 任务名) 当键——用单个字符串会让两个空间
// 里的同名任务互相覆盖。

// workspaceJSON 是一份两空间配置，两个空间各有一个同名任务 shared。
const workspaceJSON = `{
	"config_version": 4,
	"local_api_key": "k",
	"providers": {"p": {"base_url": "https://a.test", "keys": {"k": {"api_key": "sk"}}}},
	"models": {
		"model-a": {"targets": [{"provider": "p", "key": "k"}]},
		"model-b": {"targets": [{"provider": "p", "key": "k"}]}
	},
	"tasks": {"shared": {"model": "model-a", "fallback_model": "model-b", "params": {"temperature": 0.1}}},
	"workspaces": {"teamA": {"tasks": {"shared": {"model": "model-b"}}}}
}`

// TestTaskLookupIsWorkspaceScoped 固化同名任务按空间区分。
func TestTaskLookupIsWorkspaceScoped(t *testing.T) {
	pool := New(mustConfig(t, workspaceJSON), nil, nil)

	// 默认空间：shared -> model-a，带备选与固定参数。
	plan, found := pool.TaskPlanIn(config.DefaultWorkspace, "shared")
	if !found {
		t.Fatal("默认空间应有 shared 任务")
	}
	if plan.Primary.Model != "model-a" {
		t.Errorf("默认空间 shared 首选: got %s want model-a", plan.Primary.Model)
	}
	if plan.Fallback == nil || plan.Fallback.Model != "model-b" {
		t.Errorf("默认空间 shared 备选错误: %+v", plan.Fallback)
	}
	if params := pool.TaskParamsIn(config.DefaultWorkspace, "shared"); params.Obj.Len() != 1 {
		t.Errorf("默认空间 shared 应有一个固定参数，实际 %s", mustDump(t, params))
	}

	// teamA：同名但指向 model-b，且没有备选与参数。
	plan, found = pool.TaskPlanIn("teamA", "shared")
	if !found {
		t.Fatal("teamA 应有 shared 任务")
	}
	if plan.Primary.Model != "model-b" {
		t.Errorf("teamA shared 首选: got %s want model-b", plan.Primary.Model)
	}
	if plan.Fallback != nil {
		t.Errorf("teamA shared 不应有备选: %+v", plan.Fallback)
	}
	if params := pool.TaskParamsIn("teamA", "shared"); params.Obj.Len() != 0 {
		t.Errorf("teamA shared 不应有固定参数，实际 %s", mustDump(t, params))
	}

	// 未知空间查不到——不能回落到别的空间。
	if _, found := pool.TaskPlanIn("nope", "shared"); found {
		t.Error("未知空间不应查到任务")
	}
	// 空空间名等价于默认空间。
	if empty, _ := pool.TaskPlanIn("", "shared"); empty.Primary.Model != "model-a" {
		t.Errorf("空空间名应等价于默认空间，实际 %s", empty.Primary.Model)
	}
}

// TestResolveRouteInIsWorkspaceScoped 固化路由解析只在指定空间内查任务。
func TestResolveRouteInIsWorkspaceScoped(t *testing.T) {
	pool := New(mustConfig(t, workspaceJSON), nil, nil)

	model, key, err := pool.ResolveRouteIn(config.DefaultWorkspace, "shared", nil, "chat/completions")
	if err != nil || model != "model-a" || key != "" {
		t.Fatalf("默认空间解析: got (%s, %q, %v) want (model-a, \"\", nil)", model, key, err)
	}
	model, key, err = pool.ResolveRouteIn("teamA", "shared", nil, "chat/completions")
	if err != nil || model != "model-b" || key != "" {
		t.Fatalf("teamA 解析: got (%s, %q, %v) want (model-b, \"\", nil)", model, key, err)
	}
	// 未知空间里 shared 不是任务，于是按普通模型名解析——这个名字没有配置，
	// 但 resolveModelID 对未知名字是原样返回（是否可用由 KeyCount 判断）。
	model, _, err = pool.ResolveRouteIn("nope", "shared", nil, "chat/completions")
	if err != nil || model != "shared" {
		t.Fatalf("未知空间应回落到模型名解析: got (%s, %v)", model, err)
	}
	// model-a 是真实模型，在任何空间里都不该被任务规则劫持。
	if model, _, _ := pool.ResolveRouteIn("teamA", "model-a", nil, "chat/completions"); model != "model-a" {
		t.Errorf("真实模型名不应受工作空间影响，实际 %s", model)
	}
}

// TestLegacyTaskAccessorsUseDefaultWorkspace 固化既有 API 只作用于默认空间。
func TestLegacyTaskAccessorsUseDefaultWorkspace(t *testing.T) {
	pool := New(mustConfig(t, workspaceJSON), nil, nil)

	plan, found := pool.TaskPlan("shared")
	if !found || plan.Primary.Model != "model-a" {
		t.Fatalf("TaskPlan 应取默认空间: found=%v plan=%+v", found, plan)
	}
	if params := pool.TaskParams("shared"); params.Obj.Len() != 1 {
		t.Errorf("TaskParams 应取默认空间: %s", mustDump(t, params))
	}
	// teamA 的任务不该被既有 API 看到。
	if plan, found := pool.TaskPlan("teamA-only"); found {
		t.Errorf("不存在的任务不应查到: %+v", plan)
	}
}

// TestTaskParamsReturnsCopy 固化 TaskParams 返回副本，调用方改不动池内的值。
func TestTaskParamsReturnsCopy(t *testing.T) {
	pool := New(mustConfig(t, workspaceJSON), nil, nil)
	params := pool.TaskParams("shared")
	params.SetKey("temperature", canonical.NewString("mutated"))
	again := pool.TaskParams("shared")
	if value, _ := again.LookupOK("temperature"); value.StringValue() == "mutated" {
		t.Error("TaskParams 应返回副本，池内值不应被外部修改")
	}
}
