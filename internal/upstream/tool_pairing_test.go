// tool_pairing_test.go 出站工具配对三步（merge/repack/cleanup）的行为钉桩。
//
// 断言口径来自 2026-09-20 的线上实机定位：上游对「声明 tool_calls 的 assistant 之后必须
// 紧跟它自己的结果」有硬校验，违反即 400 code=11148（tool_call_sequence_broken）并顶死整条
// 会话（换号无效）。对照实验见 mergeAdjacentToolCalls 注释。
package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// msgs 解析测试用的 messages JSON 数组。
func msgs(t *testing.T, s string) []any {
	t.Helper()
	var v []any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("parse messages: %v", err)
	}
	return v
}

// summarize 把消息序列压成可读摘要：assistant 带 tool_calls 记 "assistant(id1,id2)"，
// 其余记 "role(-)"。用于逐条比对「合并后长什么样」。
func summarize(messages []any) []string {
	out := make([]string, 0, len(messages))
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			out = append(out, "<non-object>")
			continue
		}
		role, _ := msg["role"].(string)
		if role == "tool" {
			id, _ := msg["tool_call_id"].(string)
			out = append(out, "tool("+id+")")
			continue
		}
		ids := []string{}
		if tcs, ok := msg["tool_calls"].([]any); ok {
			for _, tci := range tcs {
				if tc, ok := tci.(map[string]any); ok {
					id, _ := tc["id"].(string)
					ids = append(ids, id)
				}
			}
		}
		if len(ids) > 0 {
			out = append(out, role+"("+strings.Join(ids, ",")+")")
			continue
		}
		out = append(out, role+"(-)")
	}
	return out
}

func assertSummary(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("消息数不符:\n got %v\nwant %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("消息[%d] = %s want %s\n got %v\nwant %v", i, got[i], want[i], got, want)
		}
	}
}

// TestMergeAdjacentToolCalls 背靠背的两条 assistant.tool_calls 必须合成一条。
//
// 这是部分 OpenAI 兼容 agent 客户端回放并行工具调用的报文形状（同一批调用
// 拆成多条独立 assistant 消息），上游 deepseek 系模型对它判 11148。
func TestMergeAdjacentToolCalls(t *testing.T) {
	const (
		aNil = `{"role":"assistant","content":null,"tool_calls":[{"id":"c00","type":"function","function":{"name":"f","arguments":"{}"}}]}`
		aC01 = `{"role":"assistant","content":null,"tool_calls":[{"id":"c01","type":"function","function":{"name":"f","arguments":"{}"}}]}`
		tC00 = `{"role":"tool","tool_call_id":"c00","content":"r0"}`
		tC01 = `{"role":"tool","tool_call_id":"c01","content":"r1"}`
	)

	t.Run("并行两条合成一条（修复目标形态）", func(t *testing.T) {
		// 输入即线上复现 11148 的报文：assistant(c00) 之后紧跟 assistant(c01)。
		in := msgs(t, "["+aNil+","+aC01+","+tC00+","+tC01+"]")
		out, changed := mergeAdjacentToolCalls(in)
		if !changed {
			t.Fatal("changed=false，期望发生合并")
		}
		assertSummary(t, summarize(out), []string{"assistant(c00,c01)", "tool(c00)", "tool(c01)"})
		// 拼接顺序必须是声明顺序（= 结果的 wire 顺序），不得反转。
		tcs := out[0].(map[string]any)["tool_calls"].([]any)
		if id, _ := tcs[0].(map[string]any)["id"].(string); id != "c00" {
			t.Errorf("合并后首个 tool_call = %q want c00", id)
		}
	})

	t.Run("三条连续全并", func(t *testing.T) {
		aC02 := `{"role":"assistant","content":null,"tool_calls":[{"id":"c02","type":"function","function":{"name":"f","arguments":"{}"}}]}`
		in := msgs(t, "["+aNil+","+aC01+","+aC02+"]")
		out, changed := mergeAdjacentToolCalls(in)
		if !changed {
			t.Fatal("changed=false，期望发生合并")
		}
		assertSummary(t, summarize(out), []string{"assistant(c00,c01,c02)"})
	})

	t.Run("已是规范形态原样返回且零改动", func(t *testing.T) {
		// chat 规范形态（一条 assistant 带全部 tool_calls）：不得改动，便于上层判断 no-op。
		merged := `{"role":"assistant","content":null,"tool_calls":[{"id":"c00"},{"id":"c01"}]}`
		in := msgs(t, "["+merged+","+tC00+","+tC01+"]")
		out, changed := mergeAdjacentToolCalls(in)
		if changed {
			t.Error("changed=true，规范形态不应被改动")
		}
		assertSummary(t, summarize(out), []string{"assistant(c00,c01)", "tool(c00)", "tool(c01)"})
	})

	t.Run("中间隔着消息不合并（非背靠背）", func(t *testing.T) {
		// 隔了 user 消息 = 两条独立声明，不是同一批；擅自合并会改变语义。
		in := msgs(t, "["+aNil+`,{"role":"user","content":"x"},`+aC01+","+tC00+","+tC01+"]")
		out, changed := mergeAdjacentToolCalls(in)
		if changed {
			t.Error("changed=true，非相邻不应合并")
		}
		assertSummary(t, summarize(out), []string{"assistant(c00)", "user(-)", "assistant(c01)", "tool(c00)", "tool(c01)"})
	})

	t.Run("后一条 content 非空不合并", func(t *testing.T) {
		// content 非空无法无损拼接：不猜语义，原样交给上游。
		in := msgs(t, "["+aNil+`,{"role":"assistant","content":"text","tool_calls":[{"id":"c01"}]}`+"]")
		out, changed := mergeAdjacentToolCalls(in)
		if changed {
			t.Error("changed=true，content 非空不应合并")
		}
		assertSummary(t, summarize(out), []string{"assistant(c00)", "assistant(c01)"})
	})

	t.Run("前一条无 tool_calls 不合并", func(t *testing.T) {
		in := msgs(t, `[{"role":"assistant","content":"hi"},`+aNil+"]")
		out, changed := mergeAdjacentToolCalls(in)
		if changed {
			t.Error("changed=true，前一条无 tool_calls 不应合并")
		}
		assertSummary(t, summarize(out), []string{"assistant(-)", "assistant(c00)"})
	})

	t.Run("reasoning_content 逐条保留（换行拼接）", func(t *testing.T) {
		// deepseek 多轮要求 assistant 带回填思维链，丢弃会换一个错误，故必须搬过去。
		in := msgs(t, `[{"role":"assistant","content":null,"reasoning_content":"r1","tool_calls":[{"id":"c00"}]},{"role":"assistant","content":null,"reasoning_content":"r2","tool_calls":[{"id":"c01"}]}]`)
		out, changed := mergeAdjacentToolCalls(in)
		if !changed {
			t.Fatal("changed=false，期望合并")
		}
		rc, _ := out[0].(map[string]any)["reasoning_content"].(string)
		if rc != "r1\nr2" {
			t.Errorf("reasoning_content = %q want %q", rc, "r1\nr2")
		}
	})

	t.Run("首条即 assistant.tool_calls 不越界", func(t *testing.T) {
		out, changed := mergeAdjacentToolCalls(msgs(t, "["+aNil+"]"))
		if changed {
			t.Error("单条消息不应改动")
		}
		assertSummary(t, summarize(out), []string{"assistant(c00)"})
	})

	t.Run("非对象元素原样保留且不阻断后续合并", func(t *testing.T) {
		in := msgs(t, `["str",`+aNil+","+aC01+"]")
		out, changed := mergeAdjacentToolCalls(in)
		if !changed {
			t.Fatal("changed=false，期望 aNil/aC01 合并")
		}
		assertSummary(t, summarize(out), []string{"<non-object>", "assistant(c00,c01)"})
	})
}

// TestPrepareBodyMergesSplitParallelToolCalls 全链路：出站管线必须把「拆开的并行调用」
// 归一到上游认可的形态。
//
// 断言的正是线上对照实验的结论：合成一条 assistant（本测试期望的输出形态）→ 200；
// 拆成两条 → deepseek 系模型 503/11148。
func TestPrepareBodyMergesSplitParallelToolCalls(t *testing.T) {
	body := `{"model":"deepseek-v4.1-flash","messages":[
		{"role":"user","content":"跑两个命令"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_00_a","type":"function","function":{"name":"exec_command","arguments":"{\"cmd\":\"pwd\"}"}}]},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_01_b","type":"function","function":{"name":"exec_command","arguments":"{\"cmd\":\"ls\"}"}}]},
		{"role":"tool","tool_call_id":"call_00_a","content":"/opt"},
		{"role":"tool","tool_call_id":"call_01_b","content":"a b c"}
	]}`
	out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal 出站 body: %v", err)
	}
	messages, _ := obj["messages"].([]any)
	assertSummary(t, summarize(messages), []string{
		"user(-)", "assistant(call_00_a,call_01_b)", "tool(call_00_a)", "tool(call_01_b)",
	})
	// 逐条复核：不得出现「带 tool_calls 的 assistant 紧跟另一条带 tool_calls 的 assistant」。
	prevHadCalls := false
	for i, m := range messages {
		msg, _ := m.(map[string]any)
		hasCalls := false
		if tcs, ok := msg["tool_calls"].([]any); ok && len(tcs) > 0 {
			hasCalls = true
		}
		if prevHadCalls && hasCalls {
			t.Fatalf("消息[%d] 仍是两条相邻的 assistant.tool_calls（上游判 11148）: %v", i, summarize(messages))
		}
		prevHadCalls = hasCalls
	}
}

// TestRepackToolResultBlocks 结果之间的插入消息必须挪到整组之后（上游判配对断裂的另一形态）。
func TestRepackToolResultBlocks(t *testing.T) {
	t.Run("插在结果中间的消息后移", func(t *testing.T) {
		in := msgs(t, `[
			{"role":"assistant","content":null,"tool_calls":[{"id":"c00"},{"id":"c01"}]},
			{"role":"tool","tool_call_id":"c00","content":"r0"},
			{"role":"system","content":"<image_resize_notice>"},
			{"role":"tool","tool_call_id":"c01","content":"r1"}
		]`)
		out, changed := repackToolResultBlocks(in)
		if !changed {
			t.Fatal("changed=false，期望重排")
		}
		assertSummary(t, summarize(out), []string{"assistant(c00,c01)", "tool(c00)", "tool(c01)", "system(-)"})
	})

	t.Run("下一组组头不被吞掉", func(t *testing.T) {
		// 真实会话漏排的形态：第一组结果后跟着插入消息、再接第二组组头。组头必须交还外层
		// 循环当组头处理——否则它会被当成插入物挪走，它自己那批结果（t(c10) 与 t(c11)
		// 之间夹着 system）永远得不到重排，上游判配对断裂。
		in := msgs(t, `[
			{"role":"assistant","content":null,"tool_calls":[{"id":"c00"}]},
			{"role":"tool","tool_call_id":"c00","content":"r0"},
			{"role":"system","content":"notice1"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"c10"},{"id":"c11"}]},
			{"role":"tool","tool_call_id":"c10","content":"r10"},
			{"role":"system","content":"notice2"},
			{"role":"tool","tool_call_id":"c11","content":"r11"}
		]`)
		out, changed := repackToolResultBlocks(in)
		if !changed {
			t.Fatal("changed=false，第二组的插入消息应被后移")
		}
		assertSummary(t, summarize(out), []string{
			"assistant(c00)", "tool(c00)", "system(-)",
			"assistant(c10,c11)", "tool(c10)", "tool(c11)", "system(-)",
		})
	})

	t.Run("无可重排内容时零改动", func(t *testing.T) {
		in := msgs(t, `[
			{"role":"assistant","content":null,"tool_calls":[{"id":"c00"}]},
			{"role":"tool","tool_call_id":"c00","content":"r0"}
		]`)
		_, changed := repackToolResultBlocks(in)
		if changed {
			t.Error("changed=true，规范形态不应重排")
		}
	})
}

// TestCleanupOrphanToolCalls 缺一侧的配对必须两侧同口径剔除（否则残留半截配对 → 11148）。
func TestCleanupOrphanToolCalls(t *testing.T) {
	t.Run("批内部分缺结果：调用侧对称裁剪", func(t *testing.T) {
		// 历史缺陷形态：批 [c1,c2] 只回了 c1 时整批删调用、却留下 tool{c1}，
		// 出站变成「无 tool_calls 的 assistant + 孤儿 tool」→ 11148。
		in := msgs(t, `[
			{"role":"assistant","content":null,"tool_calls":[{"id":"c1"},{"id":"c2"}]},
			{"role":"tool","tool_call_id":"c1","content":"r1"}
		]`)
		out, changed := cleanupOrphanToolCalls(in)
		if !changed {
			t.Fatal("changed=false，期望裁剪")
		}
		assertSummary(t, summarize(out), []string{"assistant(c1)", "tool(c1)"})
	})

	t.Run("调用无任何结果：删 tool_calls 键", func(t *testing.T) {
		in := msgs(t, `[{"role":"assistant","content":"hi","tool_calls":[{"id":"c1"}]}]`)
		out, changed := cleanupOrphanToolCalls(in)
		if !changed {
			t.Fatal("changed=false，期望删除 tool_calls")
		}
		msg := out[0].(map[string]any)
		if _, has := msg["tool_calls"]; has {
			t.Error("孤儿调用应删除整个 tool_calls 键")
		}
		if msg["content"] != "hi" {
			t.Errorf("content 被误改: %v", msg["content"])
		}
	})

	t.Run("孤儿结果整条删除", func(t *testing.T) {
		in := msgs(t, `[{"role":"user","content":"x"},{"role":"tool","tool_call_id":"c9","content":"r"}]`)
		out, changed := cleanupOrphanToolCalls(in)
		if !changed {
			t.Fatal("changed=false，期望删除孤儿结果")
		}
		assertSummary(t, summarize(out), []string{"user(-)"})
	})

	t.Run("完整配对零改动", func(t *testing.T) {
		in := msgs(t, `[
			{"role":"assistant","content":null,"tool_calls":[{"id":"c1"},{"id":"c2"}]},
			{"role":"tool","tool_call_id":"c1","content":"r1"},
			{"role":"tool","tool_call_id":"c2","content":"r2"}
		]`)
		out, changed := cleanupOrphanToolCalls(in)
		if changed {
			t.Error("changed=true，完整配对不应改动")
		}
		assertSummary(t, summarize(out), []string{"assistant(c1,c2)", "tool(c1)", "tool(c2)"})
	})

	t.Run("无工具流量零改动", func(t *testing.T) {
		if _, changed := cleanupOrphanToolCalls(msgs(t, `[{"role":"user","content":"x"}]`)); changed {
			t.Error("changed=true，无工具流量不应改动")
		}
	})

	t.Run("空 id 双侧不识别为工具流量", func(t *testing.T) {
		// 已知边界：id 为空的配对无法按 id 判定，两侧都不计入工具流量 → 原样透传。
		// 这是刻意保留的行为（不猜哪条结果属于哪条调用），此处钉桩以免被误改。
		in := msgs(t, `[
			{"role":"assistant","content":null,"tool_calls":[{"id":""}]},
			{"role":"tool","tool_call_id":"","content":"r"}
		]`)
		if _, changed := cleanupOrphanToolCalls(in); changed {
			t.Error("changed=true，空 id 形态当前按原样透传处理")
		}
	})
}

// TestMergeAdjacentToolCallsEmptyArrayContent 空数组 content 也必须触发合并：
// 有客户端把"没有正文"发成 `content: []`（而不是 null），此前 emptyContent 只认
// nil / 空串 → 背靠背的 assistant(tool_calls) 不合并 → 上游 deepseek 系判 11148。
func TestMergeAdjacentToolCallsEmptyArrayContent(t *testing.T) {
	in := msgs(t, `[
		{"role":"assistant","content":[],"tool_calls":[{"id":"c1","type":"function","function":{"name":"A","arguments":"{}"}}]},
		{"role":"assistant","content":[],"tool_calls":[{"id":"c2","type":"function","function":{"name":"B","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"r1"},
		{"role":"tool","tool_call_id":"c2","content":"r2"}
	]`)
	out, changed := mergeAdjacentToolCalls(in)
	if !changed {
		t.Fatal("content:[] 的两条 assistant(tool_calls) 未合并（11148 残留形态）")
	}
	if len(out) != 3 {
		t.Fatalf("合并后长度 = %d, want 3", len(out))
	}
	tcs, _ := out[0].(map[string]any)["tool_calls"].([]any)
	if len(tcs) != 2 {
		t.Fatalf("首条 tool_calls = %d, want 2", len(tcs))
	}

	// 合并只以后一条（被并入方）的 content 是否为空为判据：并入不会覆盖前一条的
	// content，所以前一条是**非空数组**时同样应当合并，且内容必须原样保留。
	in2 := msgs(t, `[
		{"role":"assistant","content":[{"type":"text","text":"hi"}],"tool_calls":[{"id":"c1","type":"function","function":{"name":"A","arguments":"{}"}}]},
		{"role":"assistant","content":[],"tool_calls":[{"id":"c2","type":"function","function":{"name":"B","arguments":"{}"}}]}
	]`)
	out2, changed2 := mergeAdjacentToolCalls(in2)
	if !changed2 {
		t.Fatal("后一条 content 为空数组时应合并")
	}
	first, _ := out2[0].(map[string]any)
	if c, ok := first["content"].([]any); !ok || len(c) != 1 {
		t.Fatalf("合并后前一条 content 丢失: %#v", first["content"])
	}
	if tcs, _ := first["tool_calls"].([]any); len(tcs) != 2 {
		t.Fatalf("合并后 tool_calls = %d, want 2", len(tcs))
	}
}

// TestFoldTextIntoPrevToolCall 反向形态：前一条是带 tool_calls 但没正文的 assistant，
// 本条是纯正文 assistant（正文排在工具调用之后）——折进前一条，合成
// assistant(正文 + tool_calls)，让"工具调用紧跟自己的结果"在两种拆分顺序下都成立。
func TestFoldTextIntoPrevToolCall(t *testing.T) {
	in := msgs(t, `[
		{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"A","arguments":"{}"}}]},
		{"role":"assistant","content":"我来解释一下"},
		{"role":"tool","tool_call_id":"c1","content":"r1"}
	]`)
	out, changed := mergeAdjacentToolCalls(in)
	if !changed {
		t.Fatal("正文未折进前一条工具调用")
	}
	if len(out) != 2 {
		t.Fatalf("合并后长度 = %d, want 2", len(out))
	}
	first, _ := out[0].(map[string]any)
	if first["content"] != "我来解释一下" {
		t.Fatalf("content = %#v, want 正文", first["content"])
	}
	if tcs, _ := first["tool_calls"].([]any); len(tcs) != 1 {
		t.Fatalf("tool_calls = %d, want 1", len(tcs))
	}
	// 结果紧跟其后，顺序合法（上游 11148 校验的关键）
	if role, _ := out[1].(map[string]any)["role"].(string); role != "tool" {
		t.Fatalf("第二条应为 tool，实际 %q", role)
	}

	// 反向守卫：前一条没有 tool_calls 时不得折叠（那是两条独立 assistant 轮次）
	in2 := msgs(t, `[
		{"role":"assistant","content":"第一轮"},
		{"role":"assistant","content":"第二轮"}
	]`)
	if _, changed2 := mergeAdjacentToolCalls(in2); changed2 {
		t.Fatal("前一条无 tool_calls 时不该折叠")
	}

	// 反向守卫：正文是数组（可能含多模态块）时不折叠，交给 repack 原样处理
	in3 := msgs(t, `[
		{"role":"assistant","content":[],"tool_calls":[{"id":"c1","type":"function","function":{"name":"A","arguments":"{}"}}]},
		{"role":"assistant","content":[{"type":"text","text":"hi"}]}
	]`)
	if _, changed3 := mergeAdjacentToolCalls(in3); changed3 {
		t.Fatal("数组正文不该被折叠（会丢结构）")
	}
}
