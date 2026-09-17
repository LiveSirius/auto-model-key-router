/*
Package agentconfig 把 AMKR 的路由信息写进第三方 AI Agent CLI 的本地配置，
让 Claude Code / Codex / Pi agent 直接连上本机 AMKR。

迁移自 auto_model_key_router/agent_config.py（541 行）。它负责三件事：

 1. **写**：把 router origin、本地鉴权 key、unified-model 等写进
    ~/.claude/settings.json、~/.codex/config.toml（+ auth.json）、
    ~/.pi/agent/models.json（agent_config.py:114 configure_agent）；
 2. **备份**：写入前把原文件内容 base64 存进缓存目录的备份 JSON，
    从而支持回退（agent_config.py:209 rollback_agent）；
 3. **状态**：通过比对当前文件与备份里的 applied_sha256 判断
    「当前配置是否就是 AMKR 写入的那份」（agent_config.py:94）。

与参照实现的两处刻意差异：home 目录是**注入**的（Options.BaseDir 替代
Path.home()，测试与对拍语料一律指向临时目录），以及 Codex 的 TOML 回写自己
实现格式保真（见下）。

# go-toml/v2 的注释丢失：实测数据

要求先测量再选型，本节记录实测结果（本机 Windows、Go 1.26.5、
github.com/pelletier/go-toml/v2 v2.4.3、Python 3.12.12、tomlkit 0.15.0）。

输入文档（3 行注释、2 处空行、1 处行尾注释、3 张表）：

	# Codex global config, hand maintained. Keep the comments!
	# (second comment line)

	model_provider = "openai"   # trailing comment
	model = "gpt-5"
	approval_policy = "on-request"

	[model_providers.openai]
	name = "OpenAI"
	base_url = "https://api.openai.com/v1"
	wire_api = "responses"

	[profiles.deep]
	model = "gpt-5-codex"
	model_reasoning_effort = "high"

(a) go-toml/v2 的 toml.Unmarshal → 改 7 个键 → toml.Marshal：

		approval_policy = 'on-request'
		model = 'unified-model'
		model_provider = 'OpenAI'
		model_reasoning_effort = 'xhigh'
		review_model = 'unified-model'

		[model_providers]
		[model_providers.OpenAI]
		base_url = 'http://127.0.0.1:8000/v1'
		name = 'OpenAI'
		requires_openai_auth = true
		wire_api = 'responses'

		[model_providers.openai]
		base_url = 'https://api.openai.com/v1'
		name = 'OpenAI'
		wire_api = 'responses'

		[profiles]
		[profiles.deep]
		model = 'gpt-5-codex'
		model_reasoning_effort = 'high'

	  - **注释行数 3 → 0**：全部注释被删除。
	  - 空行被重排（原 [profiles.deep] 前的空行消失），隐式父表 [model_providers]
	    与 [profiles] 被显式写出。
	  - 顶层键序被重排（map 迭代 + 排序），字符串引号从 " 变成 '。

结论：Unmarshal → Marshal 在用户手工维护的 config.toml 上属于**不可接受的
数据丢失**，不能作为写入路径。

(b) 真实 tomlkit（tomlkit.parse → 改同样 7 个键 → tomlkit.dumps）：

	# Codex global config, hand maintained. Keep the comments!
	# (second comment line)

	model_provider = "OpenAI"   # trailing comment
	model = "unified-model"
	approval_policy = "on-request"
	review_model = "unified-model"
	model_reasoning_effort = "xhigh"

	[model_providers.openai]
	name = "OpenAI"
	base_url = "https://api.openai.com/v1"
	wire_api = "responses"

	[model_providers.OpenAI]
	name = "OpenAI"
	base_url = "http://127.0.0.1:8000/v1"
	wire_api = "responses"
	requires_openai_auth = true
	[profiles.deep]
	model = "gpt-5-codex"
	model_reasoning_effort = "high"

注释、空行、键序、行尾注释全部保留；只有被改动的值变了，新键按 tomlkit 的规则
插入。这就是 Go 侧必须复刻的语义，也是 testdata 对拍语料的 oracle。

(c) unstable.Parser 确实能给出字节位置（Node.Raw.Offset/Length、Node.Key()、
Node.Value()），但它不暴露注释与空行节点，靠它无法重建被保留的 trivia。因此
Go 侧选择实现一个只覆盖所需子集的「迷你 tomlkit」（tomlkit.go），并**用
go-toml/v2 的 unstable parser 只做语法校验**——依赖因此有实质作用，而写入路径
完全不经过它的有损渲染。

# 刻意差异清单

每条差异都在 Go 测试里有具名断言。

D1. home 目录是注入参数。参照实现从 Path.home() 推导三个目标路径
（agent_config.py:69-84）。Go 侧改成 Options.BaseDir：默认路径由它推导，`~`
也对它展开。测试与对拍语料一律指向临时目录，**绝不触碰开发者真实的
~/.claude、~/.codex、~/.pi**。环境变量覆盖（CLAUDE_CONFIG_DIR /
PI_CODING_AGENT_DIR / CODEX_HOME）的优先级与参照实现一致。

D2. Status.Mode 用空串表示 None。合法模式只有 native / unified-model，空串不会
与合法值撞车。

D3. 错误文本。AgentConfigError 的前缀与措辞逐字保留（例如
`不支持的 Agent: x`、`Pi Agent 仅支持 unified-model 模式`、
`Codex 配置中的 model_providers 必须是 TOML 表`、`Agent 配置备份内容已损坏`）。
两类必然不同，对拍语料只断言前缀：
  - 底层解析器错误：Python 是 json.JSONDecodeError / tomlkit 错误的自身文本
    （如 `Expecting value: line 1 column 1 (char 0)`），Go 侧是 canonical /
    迷你 tomlkit / go-toml/v2 各自的文本；前缀
    （`X 配置不是有效的 UTF-8 JSON: ` / `X 配置不是有效的 UTF-8 TOML: `）逐字一致。
  - 非法 UTF-8：Python 抛 UnicodeDecodeError，Go 返回
    `... 不是有效的 UTF-8 JSON: 输入不是合法的 UTF-8`。

D4. 备份 JSON 的编码。备份用 json.dumps(indent=2, ensure_ascii=True) 写出
（agent_config.py:186）——注意是 ensure_ascii=True，与写 Agent 配置时相反，
且末尾补一个换行。canonical 包只有 ensure_ascii=False 的编码器，所以
jsonascii.go 单独实现了 ensure_ascii=True 的缩进形式（含 UTF-16 代理对）。

D5. 备份字段「缺失」的语义。参照实现混用 state.get(k)（缺省 None）与
state[k]（缺省 KeyError）；Go 侧统一成「缺失即空值 / 假值」。只有在备份文件被
人为损坏时才会观察到差异（Python 抛 KeyError，Go 当作 false / 空内容继续）。
备份由本模块自己写出，正常路径不会触发。

D6. Codex 里 model_providers / model_providers.OpenAI 的非法形状：

	用户写法                                 Python 行为                       Go 行为
	键不存在                                 新建表                            新建表（逐字节一致）
	model_providers = "x"（标量）            AgentConfigError(...必须是表)     同左，措辞逐字一致
	model_providers = [...] 或 [[...]]       TypeError（**未包装**直接冒泡）   显式 ConfigError（不支持）
	model_providers = { OpenAI = {...} }     成功，但输出「部分行内部分多行」  显式 ConfigError（不支持）
	model_providers.OpenAI = 1（点号键）     _handle_dotted_key 重建隐式表     解析期报「不是有效的 UTF-8 TOML」

最后三类在真实 Codex 配置里不出现。Go 选择**显式报错**而不是默默产生与 Python
不同（或更差）的结果——这是「不得丢失用户数据」约束下的保守取舍。

D7. Path.resolve(strict=False) vs filepath.Abs + Clean。两者对不存在的路径都只做
词法规范化，差别只在符号链接解析；AMKR 写入的三个目标路径都不应是符号链接。
对拍语料记录了 Python 的 _resolved_path 输出，Go 测试断言结果字符串一致。

D8. **合法性判定的补丁**。参照实现靠 tomlkit 做 TOML 合法性判定；Go 侧靠
go-toml/v2 的 unstable parser（validateTOML）加迷你 tomlkit 自己的检查。实测
45 份刁钻但可能出现的 TOML 文档，两者的接受/拒绝只在 3 份上不同，且方向统一是
「tomlkit 拒绝、go-toml/v2 接受」：

	model = "a" 后紧跟 model = "b"      tomlkit: ParseError          go-toml/v2: 接受
	[a] ... [a] 同一显式表头两次         tomlkit: ParseError          go-toml/v2: 接受
	[a] 里 b = 1，随后 [a.b]             tomlkit: KeyAlreadyPresent   go-toml/v2: 接受

原因是 unstable parser 只做**语法**解析，不查 TOML 的语义约束（重复键、重复表、
表与键值冲突）。放任不管的后果是：Go 会去改写一份 Python 会拒收的文件，并把重复
键原样留在用户配置里。所以迷你 tomlkit 自己拦了这三条（parseBody 的重复键检查、
attachTable/mergeInto 的重复表与表键冲突检查），语料里
codex_duplicate_key / codex_duplicate_table / codex_table_over_value 三条用例断言
「Python 报错，Go 也报同样的前缀」。

其余 42 份文档两侧判定一致；45 份的完整清单与逐条对比见本文件的实测记录（探针
脚本是临时文件，不留在仓库）。需要注意的是这仍是**采样**而非证明：如果将来发现
别的「tomlkit 拒绝而 Go 接受」的形状，应按同样方式补进迷你 tomlkit，而不是放宽
校验。

# 保留的怪癖（不是差异，是照抄参照实现的有损行为）

P1. Claude Code 的 attribution 被**整体替换**。参照实现执行
data["attribution"] = {"commit": "", "pr": ""}（agent_config.py:306），因此用户写
在 attribution 里的其它字段会丢失。Go 照抄该行为；语料里的
claude_attribution_replaced 用例断言「Python 丢什么，Go 就丢什么」。这是参照实现
自己的数据丢失点，迁移阶段刻意不改，但值得上游注意。

P2. Pi agent 的 providers.amkr 被**整体覆盖**。参照实现直接赋值
（agent_config.py:379），用户在该 provider 下的自定义字段会丢。同样照抄，由
pi_existing_providers 用例锁定。

P3. Codex 里用户自带的 [model_providers.openai]（小写）**不被覆盖**。
CODEX_PROVIDER_ID 是大小写敏感的 "OpenAI"，两者在 Codex 里是两个 provider。
这是参照实现的既有行为，由 codex_commented 用例锁定。
*/
package agentconfig
