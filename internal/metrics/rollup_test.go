package metrics

import (
	"testing"

	"github.com/Sparrived/auto-model-key-router/internal/canonical"
)

// TestSnapshotRollupMatchesGroupedQueries 用参照实现那条取数路径（_query_stats 的逐
// 分组查询 + _query_filtered_stats）校验上卷结果。
//
// query.jsonl 锁的是夹具库上的**响应体**，覆盖不到「窗口为空」「状态码为 NULL」
// 「三个归属列不全非空」这些边角；这里就地把两种取数路径的每个分组逐字段（含键序，
// 用 DumpsOrdered）比对。上卷若在合并口径、NULL 处理或键序上有偏差，都会在这里暴露：
//
//   - 合并口径：MIN/MAX 必须取极值而不是相加，status_codes 必须按状态码累加；
//   - NULL 处理：状态码为 NULL 的行要进主聚合但不进 status_codes；
//   - 键序：每个分组的键序必须与各自的 `ORDER BY dims` 一致（上卷只有一次扫描，
//     拿不到各分组自己的顺序，只能收尾重排）。
func TestSnapshotRollupMatchesGroupedQueries(t *testing.T) {
	// assertMatchesGrouped 比对一次窗口取数。
	assertMatchesGrouped := func(t *testing.T, store *Store, since, until *string) {
		t.Helper()
		groups, err := store.snapshotRollup(since, until)
		if err != nil {
			t.Fatalf("上卷失败: %v", err)
		}

		cases := []struct {
			name        string
			dims        []string
			includeNull bool
			render      func(*statsResult) *canonical.Value
			got         *canonical.Value
		}{
			{"total", nil, true,
				func(result *statsResult) *canonical.Value { return result.get(dimKey{}).dict() },
				groups.total.get(dimKey{}).dict()},
			{"caller_types", []string{"caller_type"}, true, flatStats,
				flatStats(groups.callerTypes)},
			{"models", []string{"model_id"}, true, flatStats,
				flatStats(groups.models)},
			{"requested_models", []string{"requested_model_id"}, true, flatStats,
				flatStats(groups.requestedModels)},
			{"model_requested_models", []string{"model_id", "requested_model_id"}, true, nestedStats,
				nestedStats(groups.modelRequested)},
			{"keys", []string{"model_id", "key_name"}, true, nestedStats,
				nestedStats(groups.keys)},
			{"providers", []string{"provider_id"}, false, flatStats,
				flatStats(groups.providers)},
			{"provider_pools", []string{"provider_id", "pool_name"}, false, nestedStats,
				nestedStats(groups.providerPools)},
			{"upstream_models", []string{"upstream_model_id"}, false, flatStats,
				flatStats(groups.upstreamModels)},
		}
		for _, testCase := range cases {
			reference, err := store.queryStats(testCase.dims, since, until, testCase.includeNull)
			if err != nil {
				t.Fatalf("%s: 逐分组查询失败: %v", testCase.name, err)
			}
			got, want := compact(t, testCase.got), compact(t, testCase.render(reference))
			if got != want {
				t.Errorf("%s 的上卷结果与逐分组查询不同:\n 上卷=%s\n 逐组=%s", testCase.name, got, want)
			}
		}

		// unattributed 走的是 _query_filtered_stats（attributed=false），单独比一次。
		attributedFalse := false
		filter := metricFilter{SinceCreatedAt: since, UntilCreatedAt: until, Attributed: &attributedFalse}
		unattributedWhere, unattributedParams := filter.sql()
		reference, err := store.queryFilteredStats(unattributedWhere, unattributedParams)
		if err != nil {
			t.Fatalf("unattributed: 逐分组查询失败: %v", err)
		}
		got := compact(t, groups.unattributed.get(dimKey{}).dict())
		if want := compact(t, reference.dict()); got != want {
			t.Errorf("unattributed 的上卷结果与逐分组查询不同:\n 上卷=%s\n 逐组=%s", got, want)
		}
	}

	t.Run("corpus", func(t *testing.T) {
		for _, fixture := range []struct{ db, now string }{
			{"metrics.sqlite3", "2026-07-14T12:00:00+08:00"},
			{"epoch.sqlite3", "1970-01-01T00:30:00+08:00"},
		} {
			t.Run(fixture.db, func(t *testing.T) {
				installClock(t, parsePythonISO(t, fixture.now))
				store, err := Open(copyFixture(t, fixture.db))
				if err != nil {
					t.Fatalf("打开夹具库: %v", err)
				}
				defer store.Close()

				now := nowBeijing()
				dayAgo := formatISO(addHours(now, 24))
				nowText := formatISO(now)
				// addHours 是「往前推」，负数即未来时刻；用它做下界即可得到空窗口。
				future := formatISO(addHours(now, -1))

				// 三种窗口：有数据的一天、不设下界的全部历史、以及一个空窗口
				// （下界在未来，任何行都落不进去）。
				// 空窗口要单独测：上卷这时一行都扫不到，total / unattributed 必须
				// 仍然给出零值分组，而不是缺键。
				assertMatchesGrouped(t, store, &dayAgo, &nowText)
				assertMatchesGrouped(t, store, nil, &nowText)
				assertMatchesGrouped(t, store, &future, &nowText)
			})
		}
	})

	t.Run("edge_cases", func(t *testing.T) {
		installClock(t, beijingTime(t, "2026-07-14T12:00:00+08:00"))
		store := tempStore(t)

		openai, anthropic := "openai", "anthropic"
		poolA, poolB := "pool-a", "pool-b"
		gpt4o, sonnet := "gpt-4o", "claude-3-5-sonnet"
		ok, throttled := int64(200), int64(429)

		usage := func(prompt int64) *canonical.Value {
			return canonical.NewObjectOf(
				canonical.ObjectPair{Key: "prompt_tokens", Value: canonical.NewIntValue(prompt)},
				canonical.ObjectPair{Key: "completion_tokens", Value: canonical.NewIntValue(prompt / 2)},
			)
		}
		record := func(modelID, requested, key, caller string,
			provider, pool, upstream *string, status *int64, prompt, durationMS int64) {
			t.Helper()
			params := RecordParams{
				ModelID: modelID, KeyName: key, StatusCode: status,
				Usage: usage(prompt), CallerType: caller,
				RequestedModelID: &requested, ProviderID: provider, PoolName: pool,
				UpstreamModelID: upstream, DurationMS: durationMS, FirstTokenMS: durationMS / 10,
				Workspace: "teamA",
			}
			if err := store.Record(params); err != nil {
				t.Fatalf("写入指标 (%s/%s): %v", modelID, key, err)
			}
		}

		// 归属完整（三个归属列都非空）：进 providers / provider_pools / upstream_models。
		record("gpt-4o", "TASK_A", "k1", "local", &openai, &poolA, &gpt4o, &ok, 100, 900)
		record("gpt-4o", "TASK_A", "k1", "local", &openai, &poolA, &gpt4o, &throttled, 200, 30)
		record("gpt-4o", "TASK_B", "k2", "visitor", &openai, &poolB, &gpt4o, &ok, 300, 600)
		record("claude-3-5-sonnet", "TASK_A", "k1", "workspace", &anthropic, &poolA, &sonnet, &ok, 400, 120)
		// 归属不全（pool 为空）：三个归属列的口径要求全非空，因此只算未归属。
		record("claude-3-5-sonnet", "TASK_B", "k2", "visitor", &anthropic, nil, &sonnet, &ok, 500, 240)
		// 完全没有归属：只算未归属。
		record("gpt-4o", "TASK_B", "k3", "local", nil, nil, nil, &ok, 600, 60)
		// status_code 为 NULL：要进主聚合（requests/successes/failures），但不进
		// status_codes 字典。
		record("gpt-4o", "TASK_A", "k3", "local", &openai, &poolA, &gpt4o, nil, 700, 45)

		now := nowBeijing()
		dayAgo := formatISO(addHours(now, 24))
		nowText := formatISO(now)
		future := formatISO(addHours(now, -1))
		assertMatchesGrouped(t, store, &dayAgo, &nowText)
		assertMatchesGrouped(t, store, nil, &nowText)
		assertMatchesGrouped(t, store, &future, &nowText)
	})
}
