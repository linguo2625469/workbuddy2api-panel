package responses

import (
	"encoding/json"
	"strings"
)

// namespaceMaxDepth namespace 嵌套展开的最大深度（防御恶意深嵌套）。
const namespaceMaxDepth = 4

// nsSep 模型把 namespace 拼进函数名时使用的分隔符（如 codex_app__list_threads）。
const nsSep = "__"

// customToolHint 追加到 custom 工具描述里的说明：告诉模型把原始载荷放进 input。
const customToolHint = "This is a freeform tool: put the complete raw payload, verbatim, " +
	"into the `input` string parameter. Do not wrap it in extra JSON."

// ExpandNamespaceTools 把 Responses 的 namespace 工具展开成扁平 function 列表。
//
// 新版 Codex App 用 namespace 形式声明 MCP/插件工具：
//
//	{"type":"namespace","name":"codex_app","tools":[{name:"list_threads",...}]}
//
// 上游 Chat Completions 只认扁平 function；但客户端回程是按 (name, namespace)
// 二元组派发的，所以展开时必须记住 name -> namespace，回程再补上。
//
// 返回 (扁平工具列表, name -> namespace)。同名工具只保留第一个。
func ExpandNamespaceTools(tools []any) ([]any, map[string]string) {
	flat := []any{}
	mapping := map[string]string{}
	seen := map[string]bool{}

	var collect func(entry any, depth int, ns string)
	collect = func(entry any, depth int, ns string) {
		if depth > namespaceMaxDepth {
			return
		}
		item, ok := entry.(map[string]any)
		if !ok {
			return
		}
		etype := strings.ToLower(stringOf(item["type"]))
		if etype == "namespace" {
			subs, _ := item["tools"].([]any)
			if subs == nil {
				subs, _ = item["children"].([]any)
			}
			if subs == nil {
				subs, _ = item["functions"].([]any)
			}
			childNS := stringOf(item["name"])
			if childNS == "" {
				childNS = ns
			}
			for _, sub := range subs {
				collect(sub, depth+1, childNS)
			}
			return
		}
		// namespace 内的子工具常常没有 type 字段：按 function 处理。
		if ns != "" && (etype == "" || etype == "function") {
			fn, _ := item["function"].(map[string]any)
			if fn == nil {
				fn = map[string]any{
					"name":        item["name"],
					"description": item["description"],
					"parameters":  firstNonNil(item["parameters"], item["input_schema"]),
				}
			}
			name := strings.TrimSpace(stringOf(fn["name"]))
			if name == "" || seen[name] {
				return
			}
			seen[name] = true
			mapping[name] = ns
			params := fn["parameters"]
			if params == nil {
				params = map[string]any{"type": "object", "properties": map[string]any{}}
			}
			flatFn := map[string]any{
				"type":        "function",
				"name":        name,
				"description": stringOf(fn["description"]),
				"parameters":  params,
			}
			if s, ok := fn["strict"]; ok {
				flatFn["strict"] = s
			}
			flat = append(flat, flatFn)
			return
		}
		name := stringOf(item["name"])
		if name == "" {
			if fn, ok := item["function"].(map[string]any); ok {
				name = stringOf(fn["name"])
			}
		}
		if name != "" {
			if seen[name] {
				return
			}
			seen[name] = true
			if ns != "" {
				mapping[name] = ns
			}
		}
		flat = append(flat, item)
	}

	for _, entry := range tools {
		collect(entry, 0, "")
	}
	return flat, mapping
}

// ResolveNamespacedName 把模型回传的名字解析回 (裸名, namespace)。
//
// 模型可能回 js / ns__js / ns::js 三种形态；ns__ 用精确比对，避免 namespace
// 自身含 __ 时切错（如 codex_apps__github__list_prs）。
func ResolveNamespacedName(name string, mapping map[string]string) (string, string) {
	if name == "" || len(mapping) == 0 {
		return name, ""
	}
	if ns, ok := mapping[name]; ok {
		return name, ns
	}
	if idx := strings.Index(name, "::"); idx > 0 {
		tail, head := name[idx+2:], name[:idx]
		if ns, ok := mapping[tail]; ok {
			return tail, ns
		}
		return tail, head
	}
	for tool, ns := range mapping {
		if name == ns+nsSep+tool {
			return tool, ns
		}
	}
	return name, ""
}

// StampNamespace 给回程的 function_call / custom_tool_call 补上 namespace 字段。
func StampNamespace(item map[string]any, mapping map[string]string) {
	if len(mapping) == 0 || item == nil {
		return
	}
	if s, ok := item["namespace"].(string); ok && s != "" {
		return
	}
	bare, ns := ResolveNamespacedName(stringOf(item["name"]), mapping)
	if ns != "" {
		item["name"] = bare
		item["namespace"] = ns
	}
}

// ApplyNamespaceToCalls 给一组输出项里的工具调用补 namespace，返回修正条数。
func ApplyNamespaceToCalls(items []any, mapping map[string]string) int {
	if len(mapping) == 0 {
		return 0
	}
	fixed := 0
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch stringOf(item["type"]) {
		case "function_call", "custom_tool_call":
			before := stringOf(item["namespace"])
			StampNamespace(item, mapping)
			if before == "" && stringOf(item["namespace"]) != "" {
				fixed++
			}
		}
	}
	return fixed
}

// toolsForChat 把展开后的工具列表转成上游 Chat 形态，并收集 custom 工具名。
func toolsForChat(tools []any) ([]any, map[string]bool) {
	out := make([]any, 0, len(tools))
	custom := map[string]bool{}
	for _, raw := range tools {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if strings.EqualFold(stringOf(item["type"]), "custom") {
			name := stringOf(item["name"])
			if name != "" {
				custom[name] = true
			}
			out = append(out, downgradeCustomTool(item))
			continue
		}
		out = append(out, item)
	}
	return out, custom
}

// downgradeCustomTool 把 Responses 的 custom（freeform）工具降级成 Chat function。
//
// 上游没有 freeform 工具的概念：直接透传会让模型把载荷当普通文本吐出来，
// 客户端永远收不到工具调用（实测 52 个文本 delta、0 个工具项）。
func downgradeCustomTool(tool map[string]any) map[string]any {
	desc := stringOf(tool["description"])
	extra := ""
	if format, ok := tool["format"].(map[string]any); ok {
		if def, ok := format["definition"].(string); ok && def != "" {
			extra = "\n\nGrammar:\n" + def
		}
	}
	return map[string]any{
		"type":        "function",
		"name":        stringOf(tool["name"]),
		"description": strings.TrimSpace(desc + "\n\n" + customToolHint + extra),
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"input": map[string]any{
					"type":        "string",
					"description": "Complete raw payload for this tool, verbatim.",
				},
			},
			"required": []any{"input"},
		},
	}
}

// UnwrapCustomInput 把 {"input": "..."} 参数还原成 freeform 原始字符串。
func UnwrapCustomInput(args string) string {
	if strings.TrimSpace(args) == "" {
		return ""
	}
	var parsed any
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		return args
	}
	switch v := parsed.(type) {
	case string:
		return v
	case map[string]any:
		if s, ok := v["input"].(string); ok {
			return s
		}
		if raw, ok := v["input"]; ok && raw != nil {
			if b, err := json.Marshal(raw); err == nil {
				return string(b)
			}
		}
	}
	return args
}

// firstNonNil 返回第一个非 nil 值。
func firstNonNil(vals ...any) any {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}

// stringOf 宽松取字符串（nil / 非字符串返回空串）。
func stringOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
