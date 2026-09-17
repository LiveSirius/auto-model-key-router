//go:build amkr_no_visitor

package auth

// 以 `-tags amkr_no_visitor` 编译时关闭访客功能。
//
// 等价于 Python 侧未安装 visitor extra（itsdangerous 缺席）时的状态：所有 visitor
// 分支关闭，/health 的 visitor_installed 报 false。
//
// 若产品上确实需要运行期开关，应把它作为一项明确的安全相关配置加入 Options，
// 而不是把这里的常量改成可变变量。
const visitorFeatureAvailable = false
