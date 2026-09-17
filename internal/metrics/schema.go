package metrics

// 本文件是字节级兼容的核心：SQLite 会把 CREATE TABLE / CREATE INDEX 的语句
// **原文**存进 sqlite_master.sql，任何缩进或换行差异都会让“同一个库”看起来
// 结构不同（也会让后续 dump/比对工具报差异）。所以下面的字面量是从
// auto_model_key_router/metrics.py 的 _init_schema 逐字符抄录的，不要 gofmt
// 之外的重排、不要改成 raw string、不要顺手对齐。
//
// 抄录时的三引号内容以 12 个空格为基准缩进（Python 源码所在层级），与
// metrics.py:886-909 的第 16 空格列定义一致。

// createTableSQL 对应 metrics.py:886-909。
const createTableSQL = `
            CREATE TABLE IF NOT EXISTS request_metrics (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                created_at TEXT NOT NULL,
                caller_type TEXT NOT NULL DEFAULT 'local',
                model_id TEXT NOT NULL,
                requested_model_id TEXT NOT NULL,
                provider_id TEXT,
                pool_name TEXT,
                upstream_model_id TEXT,
                key_name TEXT NOT NULL,
                status_code INTEGER,
                success INTEGER NOT NULL,
                retried INTEGER NOT NULL,
                prompt_tokens INTEGER NOT NULL DEFAULT 0,
                completion_tokens INTEGER NOT NULL DEFAULT 0,
                total_tokens INTEGER NOT NULL DEFAULT 0,
                cached_tokens INTEGER NOT NULL DEFAULT 0,
                cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0,
                cache_read_input_tokens INTEGER NOT NULL DEFAULT 0,
                first_token_ms INTEGER NOT NULL DEFAULT 0,
                duration_ms INTEGER NOT NULL DEFAULT 0
            )
            `

// backfillStatements 对应 metrics.py:910-918 的两条 UPDATE。
//
// caller_type 的历史值可能是 NULL 或早期写错的其它字符串，统一收敛到 'local'；
// requested_model_id 是后加的列，老行默认空串，用 model_id 回填。
var backfillStatements = [...]string{
	"UPDATE request_metrics SET caller_type = 'local' WHERE caller_type IS NULL OR caller_type NOT IN ('local', 'visitor')",
	"UPDATE request_metrics SET requested_model_id = model_id WHERE requested_model_id = ''",
}

// columnUpgrades 对应 metrics.py:912-927 的 _ensure_column 调用序列。
//
// 顺序有意义：老库缺列时按此顺序 ALTER，保证列序与全新建库一致（sqlite_master
// 的列序影响 SELECT * 与 table_info 的输出顺序）。名称/定义分列两段，因为它
// 同时用于 PRAGMA table_info 的存在性判断与 ALTER TABLE 的语句拼接。
var columnUpgrades = [...]struct{ Name, Definition string }{
	{"caller_type", "TEXT NOT NULL DEFAULT 'local'"},
	{"requested_model_id", "TEXT NOT NULL DEFAULT ''"},
	{"provider_id", "TEXT"},
	{"pool_name", "TEXT"},
	{"upstream_model_id", "TEXT"},
	{"cached_tokens", "INTEGER NOT NULL DEFAULT 0"},
	{"cache_creation_input_tokens", "INTEGER NOT NULL DEFAULT 0"},
	{"cache_read_input_tokens", "INTEGER NOT NULL DEFAULT 0"},
	{"first_token_ms", "INTEGER NOT NULL DEFAULT 0"},
	{"duration_ms", "INTEGER NOT NULL DEFAULT 0"},
}

// createIndexStatements 对应 metrics.py:929-947 的 7 条索引。
//
// requested_model_id / caller / provider / upstream_model 四类维度是后加的，
// 对应的查询都要按 created_at 做窗口过滤，所以索引是多列复合而非单列。
var createIndexStatements = [...]string{
	`CREATE INDEX IF NOT EXISTS idx_request_metrics_model ON request_metrics(model_id)`,
	`CREATE INDEX IF NOT EXISTS idx_request_metrics_requested_model ON request_metrics(requested_model_id)`,
	`CREATE INDEX IF NOT EXISTS idx_request_metrics_key ON request_metrics(model_id, key_name)`,
	`CREATE INDEX IF NOT EXISTS idx_request_metrics_created ON request_metrics(created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_request_metrics_caller ON request_metrics(caller_type, created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_request_metrics_provider ON request_metrics(provider_id, created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_request_metrics_upstream_model ON request_metrics(upstream_model_id, created_at)`,
}

// usageAggregates 对应 metrics.py:22-39 的 _USAGE_AGGREGATES。
//
// 17 个聚合、16 个 SUM/MIN/MAX 表达式。注意：
//   - failures 是 SUM(CASE WHEN success = 0 ...) 而非 COUNT(*) - SUM(success)，
//     两者在 success 为 NULL 时结果不同，这里必须照抄；
//   - MIN(duration_ms) / MIN(first_token_ms) 没有 COALESCE，空集时是 NULL，
//     由 _stats_from_aggregate 映射成 Python 的 None，再在 to_dict 里 `or 0`
//     渲染为 0；
//   - 这里**没有分位数聚合**（P95 等）。参照实现从未有过，别加。
const usageAggregates = `
    COUNT(*) AS requests,
    COALESCE(SUM(success), 0) AS successes,
    COALESCE(SUM(CASE WHEN success = 0 THEN 1 ELSE 0 END), 0) AS failures,
    COALESCE(SUM(success), 0) AS retries,
    COALESCE(SUM(prompt_tokens), 0) AS prompt_tokens,
    COALESCE(SUM(completion_tokens), 0) AS completion_tokens,
    COALESCE(SUM(total_tokens), 0) AS total_tokens,
    COALESCE(SUM(cached_tokens), 0) AS cached_tokens,
    COALESCE(SUM(cache_creation_input_tokens), 0) AS cache_creation_input_tokens,
    COALESCE(SUM(cache_read_input_tokens), 0) AS cache_read_input_tokens,
    COALESCE(SUM(duration_ms), 0) AS total_duration_ms,
    MIN(duration_ms) AS min_duration_ms,
    COALESCE(MAX(duration_ms), 0) AS max_duration_ms,
    COALESCE(SUM(first_token_ms), 0) AS total_first_token_ms,
    MIN(first_token_ms) AS min_first_token_ms,
    COALESCE(MAX(first_token_ms), 0) AS max_first_token_ms
`

// insertSQL 对应 metrics.py:196-233 的 INSERT。
//
// 列序与 created_at 位置都是兼容面的一部分：老库已存在，INSERT 必须按名写入
// （SQLite 允许省略有默认值/可空的列），这里显式列全部 19 个业务列。
const insertSQL = `
        INSERT INTO request_metrics (
            created_at,
            caller_type,
            model_id,
            requested_model_id,
            provider_id,
            pool_name,
            upstream_model_id,
            key_name,
            status_code,
            success,
            retried,
            prompt_tokens,
            completion_tokens,
            total_tokens,
            cached_tokens,
            cache_creation_input_tokens,
            cache_read_input_tokens,
            first_token_ms,
            duration_ms
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        `
