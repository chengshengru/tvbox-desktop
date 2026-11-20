// params.go：协议方法参数编组，与 TVBox input JSON 对齐（设计文档 §3.1/§3.3）。
//
// 作者：Shiro
package spider

// BuildInput 把业务字段编组成 TVBox input：统一附加 type 字段，
// 引擎侧再 JSON.stringify 后传给脚本 main(input) / spider 类方法。
// method ∈ {home homeVod category detail search play live}；
// ext/group 属于 init 阶段参数，由池单独注入，不在这里出现。
func BuildInput(method string, params map[string]any) map[string]any {
	input := make(map[string]any, len(params)+1)
	for k, v := range params {
		input[k] = v
	}
	input["type"] = inputType(method)
	return input
}

// inputType 桌面方法名 → TVBox input.type 取值（猫协议里分类叫 cateList）。
func inputType(method string) string {
	switch method {
	case "category":
		return "cateList"
	default:
		return method
	}
}
