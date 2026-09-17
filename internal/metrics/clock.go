// Package metrics 移植 auto_model_key_router/metrics.py 的 SQLite 指标层。
//
// 兼容性契约（硬要求）：已有的 metrics.sqlite3 必须能被本包直接读写，用户可
// 以直接换二进制。因此这里不做任何“顺手优化”：
//
//  1. 无版本号、无迁移框架。建表语句与参照实现逐字符一致，SQLite 会把
//     "CREATE TABLE IF NOT EXISTS x" 归一化存储为 "CREATE TABLE x"，两边同形。
//  2. created_at 是 datetime.now(ZoneInfo("Asia/Shanghai")).isoformat() 的结果，
//     形如 2026-07-14T12:00:00+08:00；**微秒为 0 时整段省略**。窗口过滤全部是
//     字符串字典序比较，所以这里是静默错位的高危面：绝不能改用 time.RFC3339 /
//     RFC3339Nano（前者丢微秒，后者整秒会写成 .000000000Z）。
//  3. 分桶沿用 SQLite 的 strftime('%s', ...) 算术，锚点 BUCKET_ANCHOR_EPOCH。
package metrics

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	// 精简镜像（scratch 容器）里没有 /usr/share/zoneinfo，Asia/Shanghai 会解析
	// 失败。参照实现靠系统的 tzdata（Python 的 zoneinfo 或 tzdata 包）；Go 侧
	// 内嵌一份，保证 created_at 的 +08:00 偏移在任何环境都成立。
	_ "time/tzdata"
)

const (
	// RateWindowSeconds 对应 metrics.py:17。
	RateWindowSeconds = 60
	// MaxSeriesPoints 对应 metrics.py:18。
	MaxSeriesPoints = 500
	// CountSemantics 说明每条记录代表一次上游尝试，而非一次客户端请求。
	CountSemantics = "upstream_attempt"
	// BucketAnchorEpoch 是 int(datetime(1970, 1, 1, tzinfo=BEIJING_TZ).timestamp())。
	//
	// 1970 年的上海是 +08:00（1901 年起才固定），故为 -28800。分桶锚点与
	// 参照实现必须是同一个数，否则跨桶边界的点位会整体位移。
	BucketAnchorEpoch = -28800
)

// microsecondsPerHour 是 timedelta(hours=1) 的微秒数。
const microsecondsPerHour = 3_600_000_000

// beijingTZ 是参照实现的 BEIJING_TZ。LoadLocation 失败时退化为固定 +08:00：
// 现代上海没有夏令时（1991 年后），固定偏移与 tzdata 在 1970 年后完全等价。
var beijingTZ = loadBeijing()

func loadBeijing() *time.Location {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("CST", 8*3600)
	}
	return location
}

// BeijingTZ 返回参照实现使用的时区。
func BeijingTZ() *time.Location { return beijingTZ }

// formatISO 复刻 Python 的 datetime.isoformat()。
//
// 参照实现的 datetime 来自 datetime.now(BEIJING_TZ) 或 fromtimestamp(...)，永远
// 带 tzinfo，所以输出恒为 "YYYY-MM-DDTHH:MM:SS[.ffffff]+HH:MM"：
//   - 微秒为 0 时**整段省略**（不是 ".000000"）；
//   - 微秒非 0 时补足 6 位；
//   - 偏移按 ±HH:MM 输出，带秒的偏移输出 ±HH:MM:SS。
func formatISO(t time.Time) string {
	moment := t.In(beijingTZ)
	var b strings.Builder
	b.WriteString(moment.Format("2006-01-02T15:04:05"))
	if micro := moment.Nanosecond() / 1000; micro != 0 {
		fmt.Fprintf(&b, ".%06d", micro)
	}
	writeOffset(&b, moment)
	return b.String()
}

// writeOffset 写出 Python isoformat 的时区后缀。
func writeOffset(b *strings.Builder, moment time.Time) {
	_, offset := moment.Zone()
	sign := byte('+')
	if offset < 0 {
		sign = '-'
		offset = -offset
	}
	b.WriteByte(sign)
	fmt.Fprintf(b, "%02d:%02d", offset/3600, offset%3600/60)
	if seconds := offset % 60; seconds != 0 {
		fmt.Fprintf(b, ":%02d", seconds)
	}
}

// roundHalfEven 复刻 Python 的 round(float)（无 ndigits）。
//
// Python 的 round 是**银行家舍入**：0.5 → 0、1.5 → 2、-1.5 → -2。且它作用在
// double 的精确值上，所以这里先取 floor 再看小数部分是否为精确的 0.5。
func roundHalfEven(x float64) int64 {
	floor := math.Floor(x)
	switch frac := x - floor; {
	case frac > 0.5:
		return int64(floor) + 1
	case frac < 0.5:
		return int64(floor)
	default:
		// frac == 0.5（或 x 本身是整数，此时 frac == 0 走上面分支）。
		if int64(floor)%2 == 0 {
			return int64(floor)
		}
		return int64(floor) + 1
	}
}

// pyRoundToSix 复刻 Python 的 round(float, 6)，返回舍入后的浮点值。
//
// Python 的 ndigits 舍入是在 double 的**精确十进制展开**上做最近偶数舍入，
// Go 的 strconv.FormatFloat(x, 'f', 6, 64) 规则相同（strconv/decimal.go 的
// shouldRoundUp 在恰好中点时同样取偶，不是半个远离零）。因此先定长格式化成
// 十进制字面量、再解析回来，就得到与 Python 相同的 double。之后交给
// canonical.NewFloat 按 Python repr 渲染（2.5 保持 "2.5"，不会变成
// "2.500000"）。边界由 TestRateMatchesPython 覆盖（含 2^-7 这类真正的中点）。
func pyRoundToSix(x float64) float64 {
	if math.IsNaN(x) || math.IsInf(x, 0) {
		return x
	}
	rounded, err := strconv.ParseFloat(strconv.FormatFloat(x, 'f', 6, 64), 64)
	if err != nil {
		return x
	}
	return rounded
}

// rate 对应 metrics.py:1158 的 _rate：分母 <= 0 时返回 0.0。
func rate(numerator, denominator int64) float64 {
	if denominator <= 0 {
		return 0.0
	}
	return pyRoundToSix(float64(numerator) / float64(denominator))
}

// addHours 复刻 datetime - timedelta(hours=hours)。
//
// 关键点：CPython 把 hours 拆成整数部分与小数部分，整数部分精确换算成微秒，
// 小数部分做一次**最近偶数**舍入。直接算 hours*3.6e9 再舍入会在约 0.005% 的
// 输入上差 1 微秒（实测 30 万随机样本中 88 例），进而让窗口边界上的记录
// 错进错出。timedelta 内部以 floor 归一化，故负值也走同一套。
func addHours(moment time.Time, hours float64) time.Time {
	total := timedeltaMicroseconds(hours)
	days := floorDiv(total, 86_400_000_000)
	rest := total - days*86_400_000_000
	seconds := rest / 1_000_000
	micro := rest % 1_000_000
	// days 已归一化为非负，这里按天/秒/微秒逐级回退，避免 time.Duration 溢出。
	return moment.AddDate(0, 0, -int(days)).
		Add(-time.Duration(seconds) * time.Second).
		Add(-time.Duration(micro) * time.Microsecond)
}

// timedeltaMicroseconds 返回 timedelta(hours=hours) 的微秒数。
func timedeltaMicroseconds(hours float64) int64 {
	integral, frac := math.Modf(hours)
	return int64(integral)*microsecondsPerHour + roundHalfEven(frac*microsecondsPerHour)
}

// addSeconds 复刻 datetime - timedelta(seconds=n)，n 为整数。
func addSeconds(moment time.Time, seconds int64) time.Time {
	return moment.Add(-time.Duration(seconds) * time.Second)
}

// floorDiv 是 Python 的整数地板除。
func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// unixSecondsTruncated 复刻 int(datetime.timestamp())。Python 的 int() 向零截断，
// 而 Go 的 Time.Unix() 向负无穷取整：1969-12-31T23:59:59.5+08:00 在 Python 是
// -28801.5 → -28801，在 Go 是 Unix() == -28802。分桶起点差 1 秒会错一个桶。
func unixSecondsTruncated(moment time.Time) int64 {
	seconds := moment.Unix()
	if seconds < 0 && moment.Nanosecond() > 0 {
		return seconds + 1
	}
	return seconds
}

// bucketEpoch 对应 metrics.py:1018 的 _bucket_epoch。
func bucketEpoch(moment time.Time, bucketSeconds int64) int64 {
	sinceAnchor := unixSecondsTruncated(moment) - BucketAnchorEpoch
	return floorDiv(sinceAnchor, bucketSeconds)*bucketSeconds + BucketAnchorEpoch
}

// fromUnix 对应 datetime.fromtimestamp(epoch, BEIJING_TZ)。
func fromUnix(epoch int64) time.Time {
	return time.Unix(epoch, 0).In(beijingTZ)
}

// maxBeijing 对应 datetime.max.replace(tzinfo=BEIJING_TZ)。
//
// key_stats(hours=0) 用它构造“永远为空”的窗口：用未来时间戳，避免时钟精度或
// 回拨让刚写入的记录落进窗口（metrics.py:566 的注释）。
var maxBeijing = time.Date(9999, 12, 31, 23, 59, 59, 999999000, beijingTZ)
