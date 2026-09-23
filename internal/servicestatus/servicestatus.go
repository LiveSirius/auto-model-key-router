// Package servicestatus 移植 auto_model_key_router/service_status.py：
// 采集「系统服务是否已注册及其运行状态」并解析相关命令输出。
//
// 两层结构：
//
//   - 采集层（CollectWindowsTaskStatus / CollectSystemdUserStatus）把外部命令
//     执行器作为参数注入，测试因此能在任何平台上覆盖 Windows / systemd 两条分支；
//     真正读文件的地方（systemd unit 文件）由测试把路径钉进临时目录。
//   - 解析层（Parse* 系列）是纯函数，直接吃命令输出文本。
//
// 与 Python 的已知差异都在具名测试里锁定，主要是 Go 的 encoding/xml 不做 NFKC
// 之外的 DTD 内部实体展开、以及 Python 的 expat 会拒绝「XML 声明不在文档开头」
// 这类 Go 不检查的输入（这里补齐了后者的检查）。
package servicestatus

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"
)

// StatusDetail 是状态面板里的一段折叠详情（service_status.py:13）。
//
// 字段顺序与 dataclass 一致：标题、内容、配色。
type StatusDetail struct {
	Title   string
	Content string
	Style   string
}

// Row 是状态表的一行（service_status.py:23 的 tuple[tuple[str, str], ...]）。
//
// 注意「注册状态」行的值在采集层里**恒为空串**：Python 只把 registered 放进
// 结构体，真正的「已注册/未注册」文案由渲染层（service.py:288）替换。
type Row struct {
	Label string
	Value string
}

// SystemServiceStatus 是一次系统服务状态采集的结果（service_status.py:21）。
//
// 字段顺序与 dataclass 一致：registered、rows、details。
type SystemServiceStatus struct {
	Registered bool
	Rows       []Row
	Details    []StatusDetail
}

// CommandResult 是一条状态命令的执行结果。
//
// Err 对应 Python 里 subprocess.run 抛出的异常（例如 schtasks 不存在时的
// OSError）：采集层会把它原样返回给调用方，与 Python 的异常传播一致。
type CommandResult struct {
	Stdout string
	Stderr string
	Code   int
	Err    error
}

// CommandRunner 执行一条命令，对应 Python 的 CommandRunner
// （Callable[[list[str]], subprocess.CompletedProcess[str]]，service_status.py:10）。
type CommandRunner func(command []string) CommandResult

// ErrUnitFileNotUTF8 表示 systemd unit 文件不是合法 UTF-8。
//
// Python 的 Path.read_text(encoding="utf-8") 遇到非法字节会抛 UnicodeDecodeError；
// Go 不会自动校验，这里显式检查并给出等价的失败语义。
var ErrUnitFileNotUTF8 = errors.New("systemd unit 文件不是合法 UTF-8")

// trackedTaskFields 是 parse_windows_task_xml 会收集的任务 XML 字段
// （service_status.py:211 的 tracked 集合）。
var trackedTaskFields = map[string]bool{
	"UserId":                     true,
	"RunLevel":                   true,
	"Command":                    true,
	"Arguments":                  true,
	"WorkingDirectory":           true,
	"Enabled":                    true,
	"Hidden":                     true,
	"MultipleInstancesPolicy":    true,
	"DisallowStartIfOnBatteries": true,
	"StopIfGoingOnBatteries":     true,
	"StartWhenAvailable":         true,
	"ExecutionTimeLimit":         true,
}

// CollectWindowsTaskStatus 采集 Windows 计划任务状态（service_status.py:27）。
//
// python 与 configPath 只用于在「期望 Python / 期望配置」两行里展示：Python 侧是
// Path，调用方传 str(path) 即可。命令固定是 schtasks 的两条查询，注册状态取
// 「两条命令任一返回码为 0」，XML 解析只在 /XML 成功时进行。
func CollectWindowsTaskStatus(
	python string,
	configPath string,
	taskName string,
	run CommandRunner,
) (SystemServiceStatus, error) {
	listResult, err := runCommand(run, []string{
		"schtasks", "/Query", "/TN", taskName, "/V", "/FO", "LIST",
	})
	if err != nil {
		return SystemServiceStatus{}, err
	}
	xmlResult, err := runCommand(run, []string{"schtasks", "/Query", "/TN", taskName, "/XML"})
	if err != nil {
		return SystemServiceStatus{}, err
	}

	registered := listResult.Code == 0 || xmlResult.Code == 0
	listValues := ParseKeyValueLines(listResult.Stdout)
	taskValues := map[string]string{}
	if xmlResult.Code == 0 {
		taskValues = ParseWindowsTaskXML(xmlResult.Stdout)
	}

	rows := []Row{
		{Label: "平台", Value: "Windows 计划任务"},
		{Label: "任务名", Value: taskName},
		// 值恒为空串：渲染层按 registered 填「已注册/未注册」（service.py:288）。
		{Label: "注册状态", Value: ""},
		{Label: "运行状态", Value: firstValueOr(
			listValues, "Status", "状态", "Scheduled Task State", "任务状态")},
		{Label: "触发器", Value: getOr(taskValues, "Triggers", "-")},
		{Label: "账户", Value: getOr(taskValues, "UserId",
			firstValueOr(listValues, "Run As User", "运行身份"))},
		{Label: "权限级别", Value: getOr(taskValues, "RunLevel", "-")},
		{Label: "执行程序", Value: getOr(taskValues, "Command", "-")},
		{Label: "启动参数", Value: getOr(taskValues, "Arguments", "-")},
		{Label: "工作目录", Value: getOr(taskValues, "WorkingDirectory", "-")},
		{Label: "电池启动限制", Value: getOr(taskValues, "DisallowStartIfOnBatteries", "-")},
		{Label: "电池停止", Value: getOr(taskValues, "StopIfGoingOnBatteries", "-")},
		{Label: "错过后尽快启动", Value: getOr(taskValues, "StartWhenAvailable", "-")},
		{Label: "执行时限", Value: getOr(taskValues, "ExecutionTimeLimit", "-")},
		{Label: "上次运行", Value: firstValueOr(listValues, "Last Run Time", "上次运行时间")},
		{Label: "下次运行", Value: firstValueOr(listValues, "Next Run Time", "下次运行时间")},
		{Label: "上次结果", Value: firstValueOr(listValues, "Last Result", "上次结果")},
		{Label: "期望配置", Value: configPath},
		{Label: "期望 Python", Value: python},
	}

	var details []StatusDetail
	if raw := CommandOutput(listResult); raw != "" {
		style := "blue"
		if listResult.Code != 0 {
			style = "red"
		}
		details = append(details, StatusDetail{Title: "Windows 原始状态", Content: raw, Style: style})
	} else if !registered {
		details = append(details, StatusDetail{
			Title:   "Windows 原始状态",
			Content: "未找到计划任务，或 schtasks 不可用。",
			Style:   "yellow",
		})
	}
	return SystemServiceStatus{Registered: registered, Rows: rows, Details: details}, nil
}

// CollectSystemdUserStatus 采集 Linux systemd 用户服务状态（service_status.py:93）。
//
// 与 Python 的唯一内部差异：servicePath 的「是否存在」只判断一次，而 Python 在
// 三处各判断一次（service_status.py:115/118/129）。对稳定的文件系统二者等价，
// 少一次 stat 也避免出现「行里说存在、读的时候又不存在」的自相矛盾。
//
// 读文件复刻 Path.read_text(encoding="utf-8") 的两件事：严格 UTF-8 解码（失败返回
// ErrUnitFileNotUTF8）与通用换行转换（\r\n、\r 一律换成 \n）。
func CollectSystemdUserStatus(
	python string,
	configPath string,
	servicePath string,
	serviceName string,
	run CommandRunner,
) (SystemServiceStatus, error) {
	showResult, err := runCommand(run, []string{
		"systemctl", "--user", "show", serviceName,
		"--property=LoadState,ActiveState,SubState,UnitFileState,FragmentPath," +
			"ExecMainPID,ExecMainStatus,Result,MainPID,NRestarts,NeedDaemonReload",
		"--no-pager",
	})
	if err != nil {
		return SystemServiceStatus{}, err
	}
	statusResult, err := runCommand(run, []string{"systemctl", "--user", "status", serviceName, "--no-pager"})
	if err != nil {
		return SystemServiceStatus{}, err
	}

	showValues := ParseSystemctlProperties(showResult.Stdout)
	serviceExists := pathExists(servicePath)
	unitText := ""
	if serviceExists {
		unitText, err = readUnitFile(servicePath)
		if err != nil {
			return SystemServiceStatus{}, err
		}
	}
	unitValues := ParseSystemdUnitFile(unitText)

	loadState, hasLoadState := showValues["LoadState"]
	registered := serviceExists ||
		(hasLoadState && loadState != "" && loadState != "not-found")

	unitFileRow := servicePath
	if !serviceExists {
		unitFileRow = "未找到: " + servicePath
	}
	rows := []Row{
		{Label: "平台", Value: "Linux systemd user service"},
		{Label: "服务名", Value: serviceName},
		{Label: "注册状态", Value: ""},
		{Label: "Unit 文件", Value: unitFileRow},
		{Label: "加载状态", Value: getOr(showValues, "LoadState", "-")},
		{Label: "启用状态", Value: getOr(showValues, "UnitFileState", "-")},
		// ActiveState 与 SubState 都非空时才用 "/" 连接（Python 的 join + or "-"）。
		{Label: "运行状态", Value: joinNonEmpty("/",
			showValues["ActiveState"], showValues["SubState"])},
		{Label: "Main PID", Value: firstValueOr(showValues, "MainPID", "ExecMainPID")},
		{Label: "最近结果", Value: getOr(showValues, "Result", "-")},
		{Label: "退出码", Value: getOr(showValues, "ExecMainStatus", "-")},
		{Label: "重启次数", Value: getOr(showValues, "NRestarts", "-")},
		{Label: "需 daemon-reload", Value: getOr(showValues, "NeedDaemonReload", "-")},
		{Label: "工作目录", Value: getOr(unitValues, "WorkingDirectory", "-")},
		{Label: "启动命令", Value: getOr(unitValues, "ExecStart", "-")},
		{Label: "重启策略", Value: getOr(unitValues, "Restart", "-")},
		{Label: "安装目标", Value: getOr(unitValues, "WantedBy", "-")},
		{Label: "期望配置", Value: configPath},
		{Label: "期望 Python", Value: python},
	}

	var details []StatusDetail
	if raw := CommandOutput(statusResult); raw != "" {
		style := "blue"
		if statusResult.Code != 0 {
			style = "red"
		}
		details = append(details, StatusDetail{Title: "systemd 原始状态", Content: raw, Style: style})
	}
	if unitText != "" {
		// Python 的 unit_text.strip()：整段去掉首尾空白后再展示。
		details = append(details, StatusDetail{
			Title:   "systemd 服务文件",
			Content: strings.TrimSpace(unitText),
			Style:   "blue",
		})
	}
	return SystemServiceStatus{Registered: registered, Rows: rows, Details: details}, nil
}

// readUnitFile 读 unit 文件，复刻 Path.read_text(encoding="utf-8")。
func readUnitFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("%w: %s", ErrUnitFileNotUTF8, path)
	}
	// 通用换行：Python 的 open() 默认把 \r\n 与 \r 都翻成 \n。
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	return strings.ReplaceAll(text, "\r", "\n"), nil
}

// runCommand 执行一条命令，把执行器返回的错误转成 Go 错误。
func runCommand(run CommandRunner, command []string) (CommandResult, error) {
	result := run(command)
	if result.Err != nil {
		return CommandResult{}, result.Err
	}
	return result, nil
}

// pathExists 等价于 Path.exists()。
func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// getOr 复刻 dict.get(key, default)：**键存在但值为空串时返回空串**，不回退默认值。
//
// 这与 Go 的 map 索引天然一致，但很容易被写成「空串就用默认值」，而
// parse_windows_task_xml 恰好会产出空串值（`<Command>   </Command>`），
// 那样表格里就会把缺失与空白混为一谈。
func getOr(values map[string]string, key, fallback string) string {
	if value, found := values[key]; found {
		return value
	}
	return fallback
}

// firstValue 复刻 first_value（service_status.py:196）：返回第一个非空值，都没有返回空串。
//
// Python 返回 None 表示「没有」，这里统一成空串——调用方紧接着就是 `or "-"`，
// 两者在下游不可区分。
func firstValue(values map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := values[key]; value != "" {
			return value
		}
	}
	return ""
}

// firstValueOr 是 first_value(...) or "-" 的合体。
func firstValueOr(values map[string]string, keys ...string) string {
	if value := firstValue(values, keys...); value != "" {
		return value
	}
	return "-"
}

// joinNonEmpty 复刻 `"/".join(v for v in values if v) or "-"`。
func joinNonEmpty(separator string, values ...string) string {
	var parts []string
	for _, value := range values {
		if value != "" {
			parts = append(parts, value)
		}
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, separator)
}

// CommandOutput 复刻 command_output（service_status.py:175）：
// stdout 与 stderr 取先非空者，strip 后非空就返回；都为空时，
// 返回码为 0 给一句「成功但没有内容」的提示，否则返回空串。
func CommandOutput(result CommandResult) string {
	output := result.Stdout
	if output == "" {
		output = result.Stderr
	}
	if trimmed := strings.TrimSpace(output); trimmed != "" {
		return trimmed
	}
	if result.Code == 0 {
		return "命令执行成功，但没有返回详细内容。"
	}
	return ""
}

// ParseKeyValueLines 解析 "键: 值" 形式的命令输出（service_status.py:184）。
//
// 与 ParseSystemctlProperties 的差别：键为空的行会被丢弃（这里要求键非空）。
func ParseKeyValueLines(text string) map[string]string {
	values := map[string]string{}
	for _, line := range pySplitLines(text) {
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		values[key] = strings.TrimSpace(value)
	}
	return values
}

// ParseSystemctlProperties 解析 systemctl show 的 KEY=VALUE 输出
// （service_status.py:240）。
//
// 刻意不校验键是否为空：`=abc` 会得到 {"": "abc"}，与 Python 一致
// （ParseKeyValueLines 会丢弃空键，两者不同，别互相「对齐」）。
func ParseSystemctlProperties(text string) map[string]string {
	values := map[string]string{}
	for _, line := range pySplitLines(text) {
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		values[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return values
}

// ParseSystemdUnitFile 解析 systemd unit 文件（service_status.py:250）。
//
// 先 strip 整行，跳过空行与 "#" 开头的注释行；`;` 开头的行**不**算注释
// （Python 也只认 "#"），没有 "=" 的行跳过。
func ParseSystemdUnitFile(text string) map[string]string {
	values := map[string]string{}
	for _, line := range pySplitLines(text) {
		stripped := strings.TrimSpace(line)
		if stripped == "" || strings.HasPrefix(stripped, "#") {
			continue
		}
		key, value, found := strings.Cut(stripped, "=")
		if !found {
			continue
		}
		values[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return values
}

// LocalXMLName 复刻 local_xml_name（service_status.py:236）：取右花括号之后的部分。
func LocalXMLName(tag string) string {
	if index := strings.LastIndex(tag, "}"); index >= 0 {
		return tag[index+1:]
	}
	return tag
}

// xmlField 是解析出的一个元素：局部名 + 前导文本（ElementTree 的 element.text）。
type xmlField struct {
	Local string
	Text  string
}

// ParseWindowsTaskXML 解析 schtasks /XML 的输出（service_status.py:204）。
//
// 需要复刻的 Python 细节：
//
//   - 先 lstrip 掉 BOM 再解析；
//   - 解析失败一律返回空 map（部分结果会被丢弃）；
//   - 按**文档顺序**遍历所有元素，名字以 "Trigger" 结尾的收集成 "Triggers" 列表；
//   - tracked 字段只在 element.text **非空**时用 setdefault 记入，因此
//     `<Command>   </Command>` 会留下空串，而 `<Command/>` 什么都不留；
//   - "Triggers" 这个容器名本身不以 "Trigger" 结尾，不会被收集。
func ParseWindowsTaskXML(text string) map[string]string {
	fields, err := parseXMLDocument(strings.TrimLeft(text, "\ufeff"))
	if err != nil {
		return map[string]string{}
	}
	values := map[string]string{}
	var triggers []string
	for _, field := range fields {
		if strings.HasSuffix(field.Local, "Trigger") {
			triggers = append(triggers, field.Local)
		}
		if !trackedTaskFields[field.Local] || field.Text == "" {
			continue
		}
		if _, exists := values[field.Local]; !exists {
			values[field.Local] = strings.TrimSpace(field.Text)
		}
	}
	if len(triggers) > 0 {
		values["Triggers"] = strings.Join(triggers, ", ")
	}
	return values
}

// parseXMLDocument 解析整篇 XML，按文档顺序返回所有元素。
//
// 比起 net/url 那类「stdlib 已经够用」的场景，这里必须自己走 token 流：
// encoding/xml 没有 DOM，而 Python 侧用 root.iter() 遍历全部元素。三处严格性
// 也要手工补上（Python 的 expat 会报 ParseError，Go 不会）：
//
//   - XML 声明必须位于文档最开头；
//   - 根元素之后不允许有第二个根元素或非空文本；
//   - 必须有根元素。
//
// 有意不复刻的一处：expat 会展开 DOCTYPE 里声明的内部实体，Go 的 encoding/xml
// 不支持（会报 invalid character entity）。schtasks 的输出没有 DTD，
// 该差异由具名测试锁定。
func parseXMLDocument(text string) ([]xmlField, error) {
	if index := xmlDeclarationIndex(text); index > 0 {
		return nil, errors.New("XML or text declaration not at start of entity")
	}
	decoder := xml.NewDecoder(strings.NewReader(text))
	// Python 拿到的是 str，XML 声明里的 encoding 不参与解码；Go 默认会因为
	// 非 UTF-8 的声明报错（schtasks 会写 encoding="UTF-16"），这里原样放行。
	decoder.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) {
		return input, nil
	}

	var fields []xmlField
	rootSeen := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch value := token.(type) {
		case xml.StartElement:
			if rootSeen {
				return nil, errors.New("junk after document element")
			}
			rootSeen = true
			if err := readElement(decoder, value, &fields); err != nil {
				return nil, err
			}
		case xml.CharData:
			if rootSeen && strings.TrimSpace(string(value)) != "" {
				return nil, errors.New("junk after document element")
			}
		}
	}
	if !rootSeen {
		return nil, errors.New("no element found")
	}
	return fields, nil
}

// readElement 读取 start 这个完整元素，把自身与后代按文档顺序追加到 fields。
//
// 元素的 Text 只取「紧跟开始标签、且在第一个子元素之前」的连续字符数据，
// 对应 ElementTree 的 element.text（子元素之间的文本是 tail，不在此列）。
func readElement(decoder *xml.Decoder, start xml.StartElement, fields *[]xmlField) error {
	*fields = append(*fields, xmlField{Local: start.Name.Local})
	index := len(*fields) - 1
	collectText := true
	for {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		switch value := token.(type) {
		case xml.StartElement:
			collectText = false
			if err := readElement(decoder, value, fields); err != nil {
				return err
			}
		case xml.CharData:
			if collectText {
				// 连续的多段字符数据要拼起来：CDATA、实体引用与普通文本在
				// token 流里可能分段，而 Python 的 TreeBuilder 会 join。
				(*fields)[index].Text += string(value)
			}
		case xml.EndElement:
			return nil
		}
	}
}

// xmlDeclarationIndex 返回 XML 声明（`<?xml` 后接空白或 "?"）的字节下标，没有则 -1。
//
// 用于复刻 expat 的「XML 声明必须在文档开头」检查：`<?xml-stylesheet ...?>`
// 是普通处理指令，不算声明。
func xmlDeclarationIndex(text string) int {
	for offset := 0; offset < len(text); {
		index := strings.Index(text[offset:], "<?xml")
		if index < 0 {
			return -1
		}
		index += offset
		rest := text[index+len("<?xml"):]
		if rest != "" && strings.IndexByte(" \t\r\n?", rest[0]) >= 0 {
			return index
		}
		offset = index + 1
	}
	return -1
}

// pySplitLines 复刻 Python 的 str.splitlines()（service_status.py:186 等三处使用）。
//
// Go 的 strings.Split(text, "\n") 会在这些输入上给出不同结果：单独的 \r（老 Mac
// 换行）、\v、\f、U+001C..U+001E、U+0085、U+2028、U+2029 在 Python 里都是换行，
// 而值里的这些字符不会被 strip 掉，会直接体现在解析结果里。
func pySplitLines(text string) []string {
	var lines []string
	start := 0
	for index := 0; index < len(text); {
		char := text[index]
		boundary := 0
		switch {
		case char == '\n' || char == '\v' || char == '\f' || char == '\r' ||
			(char >= 0x1c && char <= 0x1e):
			boundary = 1
			if char == '\r' && index+1 < len(text) && text[index+1] == '\n' {
				// \r\n 只算一个换行。
				boundary = 2
			}
		case char == 0x85:
			// 裸 U+0085 不是合法 UTF-8，但 Python 的 str 里仍会遇到（合法 UTF-8
			// 输入不会），这里按单字节处理以保持行为一致。
			boundary = 1
		case char == 0xc2 && index+1 < len(text) && text[index+1] == 0x85:
			boundary = 2
		case char == 0xe2 && index+2 < len(text) && text[index+1] == 0x80 &&
			(text[index+2] == 0xa8 || text[index+2] == 0xa9):
			boundary = 3
		}
		if boundary == 0 {
			index++
			continue
		}
		lines = append(lines, text[start:index])
		index += boundary
		start = index
	}
	if start < len(text) {
		lines = append(lines, text[start:])
	}
	return lines
}
