//go:build !amkr_no_visitor

package auth

// visitorFeatureAvailable 在默认编译下为真。
//
// **这是与参照实现的一处有意偏离，需要产品决策确认。**
//
// Python 侧没有可靠的「装了哪些 extra」记录，于是拿 itsdangerous（只由 visitor
// extra 安装的依赖）当运行时标记：能否 import 它 == 功能是否可用（visitor.py:9-17）。
// 这是打包系统的局限所迫，并不是一个语义标记。
//
// Go 是静态编译，不存在「可选依赖恰好缺席」这种状态：能编进二进制就一定有。因此
// 照搬其字面行为既无意义也会误导。这里的做法是：
//
//   - 默认编译进去即为可用（保持既有默认行为，用户换二进制后行为不变）；
//   - 需要裁掉时用 `-tags amkr_no_visitor` 编译（见 visitor_disabled.go），得到与
//     Python「未装 visitor extra」等价的 false——下游所有 visitor 分支都会关闭。
//
// 刻意用**编译期常量**而不是运行期配置：visitor 的判定在鉴权路径上
// （IsVisitorAPIKey），运行期可变的开关意味着一次错误的配置写入就能把访客权限打开
// 或关掉，而常量不可能被配置污染。
const visitorFeatureAvailable = true
