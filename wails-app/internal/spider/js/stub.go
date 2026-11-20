//go:build !cgo

// stub.go：CGO_ENABLED=0 构建下的空实现。
// QuickJS 依赖 CGO，纯 Go 交叉编译产物不注册 JS 引擎工厂，
// spider.Invoke 返回 ErrNoEngine，业务链路按 unsupported 语义降级，主程序不受影响。
//
// 作者：Shiro
package js

// Register 纯 Go 构建下不注册任何引擎。
func Register() {}
