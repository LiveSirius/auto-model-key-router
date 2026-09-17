package auth

// VisitorFeatureAvailable 报告访客功能是否可用。
//
// 对应 visitor.py:20 的 visitor_feature_available()。它出现在 /health 的
// visitor_installed 字段、管理接口的响应、以及 TUI 的菜单里，是一个对外可见的契约。
//
// 这个访问器刻意放在**不带构建标签**的文件里：两种编译形态（visitor.go 与
// visitor_disabled.go）各自只提供同名的常量，访问器始终存在，调用方无需关心当前
// 是哪种形态。
func VisitorFeatureAvailable() bool { return visitorFeatureAvailable }
