package keypool

import "time"

// nowUnixNano 返回当前时间的 Unix 纳秒，供 RealClock 换算成浮点秒。
//
// 单独抽出来是为了让 RealClock 只有一处时间源；测试全部注入假时钟，不依赖它。
func nowUnixNano() int64 { return time.Now().UnixNano() }
