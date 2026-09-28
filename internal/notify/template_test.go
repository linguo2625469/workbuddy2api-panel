package notify

import (
	"strings"
	"testing"
)

func TestValidateTemplateOK(t *testing.T) {
	for _, tpl := range []string{
		"",
		"纯文本",
		"账号 {{nickname}} 剩余 {{credits}}/{{credits_total}}，本次消耗 {{consume}}，时间 {{time}}",
		"{{ event }} {{message}}",
	} {
		if err := ValidateTemplate(tpl); err != nil {
			t.Fatalf("tpl=%q err=%v", tpl, err)
		}
	}
}

func TestValidateTemplateUnknown(t *testing.T) {
	err := ValidateTemplate("{{buyer_nick}} 买了 {{item}}")
	if err == nil || !strings.Contains(err.Error(), "buyer_nick") {
		t.Fatalf("err=%v", err)
	}
}

func TestValidateTemplateBadSyntax(t *testing.T) {
	if err := ValidateTemplate("单边 {{nickname"); err == nil {
		t.Fatal("残缺占位符应报错")
	}
}

func TestRenderTemplate(t *testing.T) {
	cfg := map[string]any{"template": "账号 {{nickname}} 剩 {{credits}}，消耗 {{consume}}，缺省 {{notset}}"}
	// notset 不在白名单 → 模板非法 → 回落默认
	if got := RenderTemplate(cfg, map[string]string{}, "默认"); got != "默认" {
		t.Fatalf("got=%q", got)
	}

	cfg["template"] = "账号 {{nickname}} 剩 {{credits}}，消耗 {{consume}}，{{event}}"
	got := RenderTemplate(cfg, map[string]string{
		"nickname": "小号", "credits": "120", "consume": "5", "event": "credit_report",
	}, "默认")
	if got != "账号 小号 剩 120，消耗 5，credit_report" {
		t.Fatalf("got=%q", got)
	}
}

func TestRenderTemplateEmpty(t *testing.T) {
	if got := RenderTemplate(map[string]any{}, map[string]string{}, "默认正文"); got != "默认正文" {
		t.Fatalf("got=%q", got)
	}
	if got := RenderTemplate(map[string]any{"template": "   "}, map[string]string{}, "默认正文"); got != "默认正文" {
		t.Fatalf("got=%q", got)
	}
}
