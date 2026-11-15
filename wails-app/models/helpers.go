// 宽松取值工具：TVBox/CMS 协议里同一字段经常数字/字符串混用，
// 统一从这里取，对应 TS 侧的 asArray / String(x) 等写法。
//
// 作者：Shiro
package models

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Str 任意 JSON 值转字符串；nil 返回空串，数字按原样，布尔按 true/false。
func Str(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case json.Number:
		return x.String()
	case bool:
		return strconv.FormatBool(x)
	default:
		return ""
	}
}

// Num 任意 JSON 值转数字；转不出来返回 def。
func Num(v any, def float64) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case json.Number:
		f, err := x.Float64()
		if err == nil {
			return f
		}
		return def
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err == nil {
			return f
		}
		return def
	}
	return def
}

// Bool 任意 JSON 值转布尔：CMS 里 1/"1"/true 都算真。
func Bool(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		s := strings.ToLower(strings.TrimSpace(x))
		return s == "1" || s == "true"
	default:
		return Num(v, 0) != 0
	}
}

// AsArray 对应 TS 的 asArray：数组原样、对象包一层、其余为空。
func AsArray(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case nil:
		return nil
	case map[string]any:
		return []any{x}
	default:
		return nil
	}
}

// StrSlice 任意数组转字符串数组（元素按 Str 宽松转换）。
func StrSlice(v any) []string {
	arr := AsArray(v)
	if arr == nil {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, it := range arr {
		out = append(out, Str(it))
	}
	return out
}

// Get 从 map 里按路径取一层键（等价 payload["code"] 这类写法）。
func Get(m map[string]any, key string) any {
	if m == nil {
		return nil
	}
	return m[key]
}
