// template.go 通知正文模板：{{variable}} 占位符校验与渲染（对齐 xianyu notification_template.py）。
//
// 所有渠道共用一套变量集（事件间取并集，渲染时缺省变量给"未知"——与 xianyu 口径一致）：
//   通用：nickname（账号昵称）、uid、event（事件名）、message（系统默认正文）、time
//   积分：credits（剩余积分）、credits_total（积分总额）、consume（本次/窗口消耗）
//
// 模板放在渠道 config 的 "template" 键；空串/缺失 = 用系统默认正文。
package notify

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// TemplateVars 全部合法占位符（校验白名单）。
var TemplateVars = map[string]bool{
	"nickname": true, "uid": true, "event": true, "message": true, "time": true,
	"credits": true, "credits_total": true, "consume": true,
}

var placeholderRe = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)

// ValidateTemplate 校验模板；合法返回 nil。
func ValidateTemplate(t string) error {
	matches := placeholderRe.FindAllStringSubmatch(t, -1)
	remaining := placeholderRe.ReplaceAllString(t, "")
	if strings.Contains(remaining, "{{") || strings.Contains(remaining, "}}") {
		return fmt.Errorf("占位符必须使用 {{variable}} 格式")
	}
	var unknown []string
	seen := map[string]bool{}
	for _, m := range matches {
		if !TemplateVars[m[1]] && !seen[m[1]] {
			unknown = append(unknown, m[1])
			seen[m[1]] = true
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("不支持的占位符: %s（可用 %s）", strings.Join(unknown, ", "), AvailableVars())
	}
	return nil
}

// AvailableVars 可用变量列表（校验错误提示与前端 hint 共用）。
func AvailableVars() string {
	vars := make([]string, 0, len(TemplateVars))
	for v := range TemplateVars {
		vars = append(vars, v)
	}
	sort.Strings(vars)
	return strings.Join(vars, ", ")
}

// RenderTemplate 渲染渠道 config 中的 template 键；缺失/空白/非法时回落 defaultMsg。
// ctx 缺省变量渲染为"未知"（与 xianyu 一致）。
func RenderTemplate(cfg map[string]any, ctx map[string]string, defaultMsg string) string {
	t, _ := cfg["template"].(string)
	if strings.TrimSpace(t) == "" || ValidateTemplate(t) != nil {
		return defaultMsg
	}
	return placeholderRe.ReplaceAllStringFunc(t, func(m string) string {
		name := placeholderRe.FindStringSubmatch(m)[1]
		if v, ok := ctx[name]; ok {
			return v
		}
		return "未知"
	})
}
