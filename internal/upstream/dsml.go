// dsml.go 上游正文里的「模型原生工具调用标记」修复层。
//
// 背景：本网关背后的 WorkBuddy 上游最终由 DeepSeek 系模型出结果，它的工具调用原生
// 语法不是 OpenAI 的 JSON tool_calls，而是一段带全角竖线分隔符的标记文本：
//
//	<PIPE>DSML<PIPE> calls>
//	<PIPE>DSML<PIPE> invoke name="exec_command">
//	<PIPE>DSML<PIPE> parameter name="cmd" string="true">ls -la<PIPE>DSML<PIPE> parameter>
//	</PIPE>DSML<PIPE> invoke>
//	</PIPE>DSML<PIPE> calls>
//
// （上面用 <PIPE> 占位，真实分隔符是两个 U+FF5C 全角竖线，见 markupPipe 常量。）
//
// 上游只在请求**声明了 tools** 时才把这段标记解析成结构化 tool_calls。一旦某次请求
// 没带上 tools（客户端一次性 exec 探测、auto-review 复核回合、或任何 tools 中途丢失
// 的请求），模型仍然想调工具，于是这段标记以纯文本落进 content。下游没有任何一层
// 认识它：Codex 只执行 Responses API 的结构化 function_call 条目，于是把标记当成
// 普通助手文本写进会话历史，回合直接结束、工具一次都没跑——用户看到的就是历史会话
// 里躺着一大段莫名其妙的标记，任务没有推进。
//
// 本文件是最后一道防线：在出站响应（流式与非流式）里识别这段标记，还原成标准的
// tool_calls / delta.tool_calls。
//
// 判定分两级，因为「客户端声明了哪些工具」这件事在链路上并不可靠——实测最常见的
// 泄漏场景恰恰是 tools 声明在中转环节丢失，此时请求体里根本没有 tools 名单：
//
//	严格判定（名单非空）：块内每个 invoke 的工具名都必须在名单里。
//	弱判定（名单为空）：只要求块结构完美闭合、块内不夹带正文，且每个工具名形如
//	  合法标识符（markupToolNameRe）。模型在正文里「讨论」这段语法时写出的多是
//	  <tool_name>、NAME 之类占位符，会被形态检查挡住。
//
// 两级判定共同的硬性前提（任一不满足即原文透出）：
//
//   - 标记块完整闭合，块内除 invoke/parameter 与空白外没有夹带其他正文；
//   - 本回合上游没有给出任何结构化 tool_calls（不覆盖真实调用）；
//   - 非流式路径额外要求 finish_reason 为 stop（length 表示正文被截断，不作解释）。
//
// 任何一条不满足 → 原文逐字节原样透出。修复层绝不吞字节：所有「拿不准」的分支都
// 走回吐路径。
package upstream

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// markupPipe 是模型原生工具调用标记的分隔符：两个全角竖线 U+FF5C。
const markupPipe = "\uFF5C\uFF5C"

// 标记块与内层元素的起止标记。
//
// 结束标记一律比同名的开始标记多一个 '/'，因此 markupInvokeRe / markupParamRe 里
// 「< 开头」的匹配不可能落到结束标记上。
const (
	markupOpen        = "<" + markupPipe + "DSML" + markupPipe + " calls>"
	markupClose       = "</" + markupPipe + "DSML" + markupPipe + " calls>"
	markupInvokeClose = "</" + markupPipe + "DSML" + markupPipe + " invoke>"
	markupParamClose  = "</" + markupPipe + "DSML" + markupPipe + " parameter>"
)

// markupTagPrefix 是开始标记的公共前缀（已转义，供下面两个正则复用）。
var markupTagPrefix = regexp.QuoteMeta("<" + markupPipe + "DSML" + markupPipe)

// markupInvokeRe 匹配 invoke 头：<PIPE>DSML<PIPE> invoke name="NAME">。
// markupParamRe 匹配 parameter 头：<PIPE>DSML<PIPE> parameter name="NAME" string="true">，
// 其中 string 属性可选（缺省按 JSON 解析取值）。
var (
	markupInvokeRe = regexp.MustCompile(markupTagPrefix + `\s+invoke\s+name="([^"]*)"\s*>`)
	markupParamRe  = regexp.MustCompile(markupTagPrefix + `\s+parameter\s+name="([^"]*)"(?:\s+string="(true|false)")?\s*>`)
)

// markupToolNameRe 是工具名的合理形态：各家 SDK 的工具名一律是这种标识符。
// 弱判定（客户端没给出 tools 名单）时用它兜底，挡住模型在正文里「讨论」这段语法
// 时写出的 <tool_name>、NAME 之类占位符。
var markupToolNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.:-]{0,63}$`)

// markupMaxBlockBytes 单次标记块的累积上限。正常块从几百字节（一条 ls）到几十 KB
// （一段 apply_patch）。超过上限仍未闭合 → 判定「这不是标记」（例如模型在正文里
// 讨论这段语法），立即原文回吐并退出块态，避免无限缓冲。
const markupMaxBlockBytes = 256 * 1024

// MarkupCall 是一次从正文里还原出来的工具调用。
type MarkupCall struct {
	Name      string
	Arguments string // JSON 对象文本
}

// MarkupRepair 是流式与非流式共用的标记修复状态机。
// 非并发安全：一条响应流一个实例。零值不可用，请用 NewMarkupRepair 构造。
type MarkupRepair struct {
	allowed      map[string]bool
	allowUnknown bool
	hold         string // 已确认可能属于标记、但还不足以判定的尾部缓冲
	inBlock      bool   // 已消费掉 markupOpen，正在块内部累积
	seen         int    // 识别到的标记块数（含被拒绝的）
	rejected     int    // 识别到但未转换的块数（观测/排障用）
	converted    int    // 已还原的调用数（观测用）
}

// NewMarkupRepair 按客户端声明的工具名构造修复器。
//
// allowed 为请求体里提取到的工具名集合（可能为空——tools 声明在中转环节丢失是常态）。
// allowUnknown 为 true 时启用弱判定：名单为空也尝试修复，改由「块结构完美 + 工具名
// 形如标识符」把关。见文件头「判定分两级」。
//
// 两者都为空/为 false → 修复器整体禁用（Enabled() 为 false），Feed 恒等透传。
func NewMarkupRepair(allowed map[string]bool, allowUnknown bool) *MarkupRepair {
	return &MarkupRepair{allowed: allowed, allowUnknown: allowUnknown}
}

// Enabled 报告修复器是否启用。
func (r *MarkupRepair) Enabled() bool {
	return r != nil && (len(r.allowed) > 0 || r.allowUnknown)
}

// Converted 返回本次流里已还原的工具调用总数（观测用）。
func (r *MarkupRepair) Converted() int {
	if r == nil {
		return 0
	}
	return r.converted
}

// Seen 返回本次流里识别到的标记块数（含被拒绝的）。
func (r *MarkupRepair) Seen() int {
	if r == nil {
		return 0
	}
	return r.seen
}

// Rejected 返回识别到但未转换的块数。Seen 与 Converted 的差额即此值；
// 单独暴露是为了排障时能一眼区分「没识别到」与「识别到但判定不通过」。
func (r *MarkupRepair) Rejected() int {
	if r == nil {
		return 0
	}
	return r.rejected
}

// Feed 吃进一段正文增量，返回「应当作为正文透出的部分」与「还原出来的工具调用」。
// 调用方必须把返回的正文按原顺序写出，并把返回的调用按出现顺序转成 tool_call。
//
// 不变量：所有 Feed 的入参拼接后，等于「所有返回的正文 + Flush() + 被成功转换的
// 标记块原文」。即除了被转换掉的标记块本身，一个字节都不会丢。
func (r *MarkupRepair) Feed(text string) (string, []MarkupCall) {
	if !r.Enabled() {
		return text, nil
	}
	s := r.hold + text
	r.hold = ""
	var emit strings.Builder
	var calls []MarkupCall
	for {
		if !r.inBlock {
			i := markupStartCandidate(s)
			if i < 0 {
				emit.WriteString(s)
				break
			}
			if len(s)-i < len(markupOpen) {
				// 起点被截断在增量边界（上游把标记切成了两片）：留到下一片再判定。
				emit.WriteString(s[:i])
				r.hold = s[i:]
				break
			}
			emit.WriteString(s[:i])
			s = s[i+len(markupOpen):]
			r.inBlock = true
			continue
		}
		k := strings.Index(s, markupClose)
		if k < 0 {
			if len(s) > markupMaxBlockBytes {
				// 超长未闭合：不是标记，原文回吐并退出块态。
				emit.WriteString(markupOpen)
				emit.WriteString(s)
				r.inBlock = false
				break
			}
			r.hold = s
			break
		}
		inner := s[:k]
		s = s[k+len(markupClose):]
		r.inBlock = false
		r.seen++
		if got, ok := parseMarkupBlock(inner, r.allowed); ok {
			calls = append(calls, got...)
			r.converted += len(got)
			continue
		}
		// 块内不是纯调用（夹带正文 / 工具名不合规 / 参数畸形）：原文回吐，绝不吞字节。
		r.rejected++
		emit.WriteString(markupOpen)
		emit.WriteString(inner)
		emit.WriteString(markupClose)
	}
	return emit.String(), calls
}

// Flush 在响应流结束时调用：把仍未判定的尾部缓冲原文交还。未闭合的块连同起始标记
// 一起回吐——上游发了什么就透出什么。
func (r *MarkupRepair) Flush() string {
	if !r.Enabled() {
		return ""
	}
	out := r.hold
	if r.inBlock {
		out = markupOpen + r.hold
	}
	r.hold = ""
	r.inBlock = false
	return out
}

// markupStartCandidate 返回 s 中最早的「可能是 markupOpen 起点」的下标：该位置之后
// 要么正好是完整的 markupOpen，要么是 markupOpen 的一个前缀（被增量边界截断）。
// 没有候选返回 -1。
func markupStartCandidate(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] != '<' {
			continue
		}
		rest := s[i:]
		if len(rest) >= len(markupOpen) {
			if rest[:len(markupOpen)] == markupOpen {
				return i
			}
			continue
		}
		if strings.HasPrefix(markupOpen, rest) {
			return i
		}
	}
	return -1
}

// parseMarkupBlock 解析 calls 块内部（外层起止标记之间的内容）。
//
// 成功要求：块内除 invoke 与空白外没有别的字节；每个 invoke 都闭合；每个 invoke 的
// 参数列表合法；工具名判定通过。工具名判定按 allowed 是否为空分两级（见文件头）：
// 名单非空走严格比对，名单为空走形态检查。任一不满足返回 ok=false，由调用方原文回吐。
func parseMarkupBlock(inner string, allowed map[string]bool) ([]MarkupCall, bool) {
	strict := len(allowed) > 0
	var out []MarkupCall
	pos := 0
	for {
		loc := markupInvokeRe.FindStringSubmatchIndex(inner[pos:])
		if loc == nil {
			break
		}
		if strings.TrimSpace(inner[pos:pos+loc[0]]) != "" {
			return nil, false // invoke 之前夹带了正文
		}
		name := inner[pos+loc[2] : pos+loc[3]]
		bodyStart := pos + loc[1]
		end := strings.Index(inner[bodyStart:], markupInvokeClose)
		if end < 0 {
			return nil, false // invoke 未闭合
		}
		body := inner[bodyStart : bodyStart+end]
		pos = bodyStart + end + len(markupInvokeClose)
		args, ok := parseMarkupParams(body)
		if !ok {
			return nil, false
		}
		if strict {
			if !allowed[name] {
				return nil, false // 工具名不在客户端声明的 tools 里 → 不认，原文透出
			}
		} else if !markupToolNameRe.MatchString(name) {
			return nil, false // 名单缺失时的弱判定：名字不像工具名 → 不认，原文透出
		}
		out = append(out, MarkupCall{Name: name, Arguments: args})
	}
	if len(out) == 0 {
		return nil, false
	}
	if strings.TrimSpace(inner[pos:]) != "" {
		return nil, false // 最后一个 invoke 之后夹带了正文
	}
	return out, true
}

// parseMarkupParams 解析一个 invoke 内部的 parameter 列表，产出 JSON 对象文本。
//
// string="true" 的取值按原文字符串处理（走 JSON 转义，命令里的换行/引号/反斜杠
// 原样保留）；其余先按 JSON 解析，失败则退化为字符串——参数值永远不丢，最坏情况
// 是类型不如模型本意。无参数的工具产出 "{}"。
func parseMarkupParams(body string) (string, bool) {
	args := map[string]any{}
	pos := 0
	for {
		loc := markupParamRe.FindStringSubmatchIndex(body[pos:])
		if loc == nil {
			break
		}
		if strings.TrimSpace(body[pos:pos+loc[0]]) != "" {
			return "", false
		}
		name := body[pos+loc[2] : pos+loc[3]]
		isString := loc[4] >= 0 && body[pos+loc[4]:pos+loc[5]] == "true"
		valStart := pos + loc[1]
		end := strings.Index(body[valStart:], markupParamClose)
		if end < 0 {
			return "", false
		}
		raw := body[valStart : valStart+end]
		pos = valStart + end + len(markupParamClose)
		if isString {
			args[name] = raw
			continue
		}
		var v any
		if json.Unmarshal([]byte(strings.TrimSpace(raw)), &v) == nil {
			args[name] = v
		} else {
			args[name] = raw
		}
	}
	if strings.TrimSpace(body[pos:]) != "" {
		return "", false
	}
	out, err := json.Marshal(args)
	if err != nil {
		return "", false
	}
	return string(out), true
}

// ChatToolDecl 是请求体顶层工具声明的投影（OpenAI/Responses/旧版 functions 三形态）。
// 导出供 server 侧单次解析投影复用（chatPeek.Tools / Functions）。
type ChatToolDecl struct {
	Name     string `json:"name"`
	Function *struct {
		Name string `json:"name"`
	} `json:"function"`
}

// ToolNamesFromPeek 从**已解析**的请求投影收集工具名集合（语义与
// ToolNameAllowlist 完全一致，区别只是不再重新 unmarshal 整份 body——handler 侧
// 已为别的诉求扫过一遍，长历史消息重扫是热路径上的纯浪费）。
//
// tools/functions 取顶层声明；messages 为逐条 RawMessage，本函数只解 tool_calls /
// function_call 两个字段，解不开的畸形条目跳过。
//
// 两个来源都为空 → 返回 nil（由调用方决定是否启用弱判定，见 NewMarkupRepair）。
func ToolNamesFromPeek(tools, functions []ChatToolDecl, messages []json.RawMessage) map[string]bool {
	names := map[string]bool{}
	for _, t := range tools {
		if t.Function != nil && t.Function.Name != "" {
			names[t.Function.Name] = true
			continue
		}
		if t.Name != "" {
			names[t.Name] = true
		}
	}
	for _, f := range functions {
		if f.Name != "" {
			names[f.Name] = true
		}
	}
	for _, raw := range messages {
		var m struct {
			ToolCalls []struct {
				Function *struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tool_calls"`
			FunctionCall *struct {
				Name string `json:"name"`
			} `json:"function_call"`
		}
		if json.Unmarshal(raw, &m) != nil {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.Function != nil && tc.Function.Name != "" {
				names[tc.Function.Name] = true
			}
		}
		if m.FunctionCall != nil && m.FunctionCall.Name != "" {
			names[m.FunctionCall.Name] = true
		}
	}
	if len(names) == 0 {
		return nil
	}
	return names
}

// ToolNameAllowlist 从客户端请求体里收集「可用于判定工具调用」的工具名集合。
//
// 两个来源都要，因为二者会分别缺失：
//  1. 本请求声明的工具：OpenAI tools[].function.name、Responses 风格裸 tools[].name、
//     旧版 functions[].name。tools 声明在中转环节丢失是实测常态。
//  2. 会话历史里出现过的工具：messages[].tool_calls[].function.name 与旧版
//     messages[].function_call.name。多轮会话里这份名单几乎总在，且天然可信——
//     它是客户端自己写进历史的工具名。
//
// 两个来源都为空 → 返回 nil，由调用方决定是否启用弱判定（见 NewMarkupRepair）。
// 解析失败按空处理（不编造名单）。
//
// 仅需工具名且手上已有解析投影的热路径应直接调 ToolNamesFromPeek，不要再扫 body。
func ToolNameAllowlist(body []byte) map[string]bool {
	var req struct {
		Tools     []ChatToolDecl    `json:"tools"`
		Functions []ChatToolDecl    `json:"functions"`
		Messages  []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil {
		return nil
	}
	return ToolNamesFromPeek(req.Tools, req.Functions, req.Messages)
}

// RepairAggregatedResponse 在非流式聚合响应里把正文中的标记还原为 tool_calls。
//
// 只在「该 choice 没有结构化 tool_calls」且「finish_reason 为 stop」时生效：前者
// 保证不覆盖真实调用，后者保证正文没有被 max_tokens 截断（截断的正文里标记必然
// 不完整，本来也解析不出来，这里只是把语义写明确）。
//
// 工具名判定按 allowed/allowUnknown 走严格或弱判定（见 NewMarkupRepair）。
// 返回本次使用的修复器，调用方据 Converted/Seen/Rejected 记观测。
func RepairAggregatedResponse(resp map[string]any, allowed map[string]bool, allowUnknown bool) *MarkupRepair {
	r := NewMarkupRepair(allowed, allowUnknown)
	if !r.Enabled() {
		return r
	}
	choices, ok := resp["choices"].([]any)
	if !ok {
		return r
	}
	for _, ci := range choices {
		c, ok := ci.(map[string]any)
		if !ok {
			continue
		}
		msg, ok := c["message"].(map[string]any)
		if !ok {
			continue
		}
		if tcs, ok := msg["tool_calls"].([]any); ok && len(tcs) > 0 {
			continue
		}
		if fr, _ := c["finish_reason"].(string); fr != "stop" {
			continue
		}
		text, _ := msg["content"].(string)
		if text == "" || !strings.Contains(text, markupPipe) {
			continue
		}
		emit, calls := r.Feed(text)
		emit += r.Flush()
		if len(calls) == 0 {
			continue
		}
		msg["content"] = emit
		msg["tool_calls"] = markupMessageToolCalls(calls)
		c["finish_reason"] = "tool_calls"
	}
	return r
}

// markupMessageToolCalls 把还原出来的调用转成**非流式** message.tool_calls 数组。
// 非流式形态不带 index（index 是流式 delta 的分片归属标记，写进完整消息里属于
// 多余字段，严格校验的客户端会挑刺）。
func markupMessageToolCalls(calls []MarkupCall) []any {
	out := make([]any, 0, len(calls))
	for i, call := range calls {
		out = append(out, map[string]any{
			"id":   markupCallID(i),
			"type": "function",
			"function": map[string]any{
				"name":      call.Name,
				"arguments": call.Arguments,
			},
		})
	}
	return out
}

// markupToolCalls 把还原出来的调用转成**流式** delta.tool_calls 数组。
// base 是首个 index（流式路径用它避开上游已用过的 index）。
//
// index 写成 float64 而不是 int：上游帧经 json.Unmarshal 进来，数值一律是 float64，
// 下游（stripToolCallNames / writeChunk 的最大 index 统计）按 float64 读。合成帧若用
// int，那些读取会静默失败并退回默认值 0——两个调用会被当成同一个 index，第二个的
// function.name 被当作「同 index 的后续分片」删掉。
func markupToolCalls(calls []MarkupCall, base int) []any {
	out := make([]any, 0, len(calls))
	for i, call := range calls {
		out = append(out, map[string]any{
			"index": float64(base + i),
			"id":    markupCallID(base + i),
			"type":  "function",
			"function": map[string]any{
				"name":      call.Name,
				"arguments": call.Arguments,
			},
		})
	}
	return out
}

// markupCallID 生成一个形态接近 OpenAI 的 tool_call id。序号参与其中，保证同一条
// 响应里多个调用的 id 互不相同。
func markupCallID(idx int) string {
	return fmt.Sprintf("call_%d_%d", time.Now().UnixNano(), idx)
}
