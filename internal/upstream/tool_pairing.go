// tool_pairing.go 出站请求体的孤儿 tool_call↔tool 配对清理 + tool 结果块重排
// （吸收参考仓库 sse.ts:91-123 resolveToolPairing 语义，适配网关的 OpenAI wire 消息形态）。
//
// 背景：OpenAI 兼容协议要求带 tool_calls 的 assistant 消息，其每一个 tool_call id
// 都必须有对应的一条 role:tool 结果消息；反之 role:tool 消息也必须有对应的前置
// tool_call。缺任一侧，上游都会以 HTTP 400 拒绝整个请求。
//
// 工具执行失败时（参数非法、超时、工具不存在……）客户端会把 assistant 的 tool_calls
// 持久化进会话历史，却写不回结果消息。这条坏历史随后被每次请求原样重放——上游对之后
// 每一条用户消息都返回 400，整条会话报废。网关是最后一道防线：发出请求前剔除无法配对
// 的条目让会话自愈，宁可丢一轮工具上下文，也好过整条会话死亡。
package upstream

// repackToolResultBlocks 把插在 assistant.tool_calls 与其 tool 结果之间的非 tool 消息
// 挪到整组之后，保证同一批 tool_call 的结果在 wire 上连续。
//
// 背景：Codex 的 image_resize_notice 特性会把 <image_resize_notice> 作为一条
// developer/system 消息插在 tool 输出后面。并行调用时它插在两份 tool 结果中间：
//
//	assistant tool_calls=[c00 c01]
//	tool c00
//	developer <image_resize_notice>   <- 插在中间
//	tool c01
//
// OpenAI 兼容协议要求 tool 结果紧跟 assistant，中间插任何消息都算配对断裂，上游判
// 11148（tool_call_sequence_broken）并顶死整条会话。这里只调顺序、不改内容：
//
//	assistant tool_calls=[c00 c01] | tool c00 | X | tool c01
//	→ assistant tool_calls=[c00 c01] | tool c00 | tool c01 | X
//
// 结果顺序保持不变（同批 tool_call 的原相对顺序 = 结果顺序），不引入新的顺序敏感
// 问题。无插入消息时零改动零分配（返回原 slice）。
func repackToolResultBlocks(messages []any) ([]any, bool) {
	if len(messages) < 3 {
		return messages, false
	}
	out := make([]any, 0, len(messages))
	changed := false
	i := 0
	for i < len(messages) {
		m, ok := messages[i].(map[string]any)
		if !ok || m["role"] != "assistant" {
			out = append(out, messages[i])
			i++
			continue
		}
		tcs, hasCalls := m["tool_calls"].([]any)
		if !hasCalls || len(tcs) == 0 {
			out = append(out, messages[i])
			i++
			continue
		}
		want := map[string]bool{}
		for _, tci := range tcs {
			if tc, ok := tci.(map[string]any); ok {
				if id, _ := tc["id"].(string); id != "" {
					want[id] = true
				}
			}
		}
		// 收集紧随其后（允许被其他消息打断）的同批 tool 结果，按原相对顺序。
		out = append(out, messages[i])
		i++
		var results []any
		var between []any
		sawNonTool := false
		for i < len(messages) {
			mm, ok := messages[i].(map[string]any)
			if !ok {
				break
			}
			role, _ := mm["role"].(string)
			if role == "tool" {
				id, _ := mm["tool_call_id"].(string)
				if !want[id] {
					break
				}
				results = append(results, messages[i])
				if sawNonTool {
					changed = true
				}
				i++
				continue
			}
			if len(results) == 0 {
				break // assistant 后没有结果：交由 cleanupOrphanToolCalls 处理
			}
			// 下一组 assistant.tool_calls 是新的组头，绝不能当插入物吞掉：一旦被收进
			// between，它永远不再被外层循环当作组头处理，它自己那批结果也就永远得不
			// 到重排（真实会话 msg[181] 正是这样漏掉的）。必须 break 交还外层循环。
			if role == "assistant" {
				if next, _ := mm["tool_calls"].([]any); len(next) > 0 {
					break
				}
			}
			// 同批结果尚未收齐时，中间消息视为插入物，暂存待后移。
			between = append(between, messages[i])
			sawNonTool = true
			i++
		}
		out = append(out, results...)
		out = append(out, between...)
	}
	if !changed {
		return messages, false
	}
	return out, true
}

// cleanupOrphanToolCalls 剔除无法配对的 tool_call 与 tool 结果（所有模型，独立于
// deepseek-only 的 sanitize 开关）。语义对齐参考仓库 resolveToolPairing：
//
//   - 收集全线 role:tool 消息的 tool_call_id（结果集）与 assistant.tool_calls[].id（调用集）；
//   - 一批 assistant.tool_calls 按 keepCalls 对称裁剪：只留有结果配对的调用（部分保留
//     不会留下无结果的 tool_call），过滤后为空才删掉整个 tool_calls 键；
//   - role:tool 只在对应 tool_call 被保留时才保留，否则删除整条消息；
//   - 无任何工具流量 → 原 slice 原样返回，changed=false（零分配零改动）。
//
// 这是「让请求通过」的安全网：只要存在合法配对就整段保留这些字段，绝不吞掉正确配对。
// 返回清理后的 slice（无改动时等于原 slice，勿依赖其是否新分配）及是否发生删除。
func cleanupOrphanToolCalls(messages []any) ([]any, bool) {
	if len(messages) == 0 {
		return messages, false
	}
	callIDs := map[string]bool{}
	resultIDs := map[string]bool{}
	hasTraffic := false
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		switch msg["role"] {
		case "tool":
			if id, ok := msg["tool_call_id"].(string); ok && id != "" {
				resultIDs[id] = true
				hasTraffic = true
			}
		case "assistant":
			if tcs, ok := msg["tool_calls"].([]any); ok {
				for _, tci := range tcs {
					tc, ok := tci.(map[string]any)
					if !ok {
						continue
					}
					if id, ok := tc["id"].(string); ok && id != "" {
						callIDs[id] = true
						hasTraffic = true
					}
				}
			}
		}
	}
	if !hasTraffic {
		return messages, false
	}
	// keepCalls：调用 id 是否双侧齐全（调用存在且结果存在）。重复 id 与乱序均按集合处理。
	keepCalls := map[string]bool{}
	for id := range callIDs {
		if resultIDs[id] {
			keepCalls[id] = true
		}
	}
	changed := false
	// 1) assistant.tool_calls：按 keepCalls 对称裁剪——只留有结果的调用，过滤后为空则删键。
	//
	// 历史实现是「批内每个 id 都齐才整批保留，否则删掉整个 tool_calls 键」。那会留下
	// 无主结果：批 [c1,c2] 只回了 c1 时，调用侧整批被删，而 tool{c1} 仍按 id 命中
	// keepCalls 得以保留 —— 出站载荷于是变成「无 tool_calls 的 assistant + 孤儿 tool」，
	// 上游判 11148（tool calls and tool results do not match）并顶死整条会话。
	// 现在两侧共用同一份 keepCalls 按 id 对称裁剪（与 2) 的删除侧同口径），
	// 任何输入都不会再产生半截配对。
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role != "assistant" {
			continue
		}
		tcs, ok := msg["tool_calls"].([]any)
		if !ok || len(tcs) == 0 {
			continue
		}
		keptCalls := make([]any, 0, len(tcs))
		for _, tci := range tcs {
			tc, ok := tci.(map[string]any)
			if !ok {
				continue
			}
			if id, _ := tc["id"].(string); keepCalls[id] {
				keptCalls = append(keptCalls, tc)
			}
		}
		if len(keptCalls) == len(tcs) {
			continue // 整批齐全：零改动
		}
		changed = true
		if len(keptCalls) == 0 {
			delete(msg, "tool_calls")
			continue
		}
		msg["tool_calls"] = keptCalls
	}
	// 2) role:tool 结果：只有对应 tool_call 被保留才保留；孤儿结果整条删除。
	kept := make([]any, 0, len(messages))
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			kept = append(kept, m)
			continue
		}
		if role, _ := msg["role"].(string); role == "tool" {
			id, _ := msg["tool_call_id"].(string)
			if !keepCalls[id] {
				changed = true
				continue
			}
		}
		kept = append(kept, m)
	}
	if !changed {
		return messages, false
	}
	return kept, true
}

// mergeAdjacentToolCalls 把**背靠背**的 assistant.tool_calls 消息合成一条（tool_calls 依原序拼接）。
//
// 2026-09-20 实机定位并复现的 11148 事故根因：部分 OpenAI 兼容 agent 客户端回放历史时把
// 同一批并行工具调用拆成多条紧邻的独立 assistant 消息，出站载荷长成：
//
//	assistant tool_calls=[c00]
//	assistant tool_calls=[c01]
//	tool c00
//	tool c01
//
// 上游要求「声明 tool_calls 的 assistant 之后必须紧跟它自己的结果」——紧随的若是另一条带
// tool_calls 的 assistant，即返回 400 code=11148（extError tool_call_sequence_broken，
// "tool calls and tool results do not match, please start a new conversation and retry"），
// 整条会话报废：客户端每次重试重放同一条历史，池侧换号也无效（不是账号问题）。
//
// 对照实验（2026-09-20 在线上网关实测，同一批调用）：
//   - 拆成两条 assistant（Codex Desktop 报文形状）+ deepseek-v4.1-flash → 503 / 11148
//   - 合成一条 assistant（=本函数产物）      + deepseek-v4.1-flash → 200
//   - 拆成两条 assistant                     + glm-5.3-flash      → 200（该模型宽容）
//
// 即：**行为本身合法**（上述第一种形态在 OpenAI 规范里也说得通），是上游 deepseek 系模型的
// 校验更严。网关作为最后一道防线按最严口径归一，客户端不必感知。
//
// 合并条件从严，避免引入新语义：
//   - 两条消息**相邻**（中间隔着任何消息都不合并——隔着消息说明不是同一批声明，
//     凭猜测合并会改变语义，这类形态按其原样交给 repackToolResultBlocks 处理）；
//   - 后一条 content 为空（content 非空无法无损拼接，不猜语义）；
//   - 前一条本身必须是带 tool_calls 的 assistant（否则不合并，例如 assistant 文本 + 独立工具调用消息）。
//
// reasoning_content（thinking.go 的多轮回填字段）不丢：后一条有则搬到合并结果，两边都有则换行拼接
// （deepseek 多轮要求 assistant 带思维链回填，丢弃会换一个错误）。
func mergeAdjacentToolCalls(messages []any) ([]any, bool) {
	if len(messages) < 2 {
		return messages, false
	}
	out := make([]any, 0, len(messages))
	changed := false
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			out = append(out, m)
			continue
		}
		if role, _ := msg["role"].(string); role == "assistant" && len(out) > 0 {
			// 形态一：本条是带 tool_calls 的 assistant 且没有正文 → 并入上一条
			// 同为 assistant 且带 tool_calls 的消息（背靠背的并行调用声明）。
			if tcs, ok := msg["tool_calls"].([]any); ok && len(tcs) > 0 && emptyContent(msg["content"]) {
				if prev, ok := out[len(out)-1].(map[string]any); ok {
					if prevRole, _ := prev["role"].(string); prevRole == "assistant" {
						if prevCalls, ok := prev["tool_calls"].([]any); ok && len(prevCalls) > 0 {
							prev["tool_calls"] = append(prevCalls, tcs...)
							mergeReasoningContent(prev, msg)
							changed = true
							continue // 本条已并入上一条，不再单独出站
						}
					}
				}
			}
			// 形态二（反向）：本条是纯正文 assistant，上一条是带 tool_calls 但没正文的
			// assistant → 把正文折进上一条，合成 assistant(正文 + tool_calls)。这样
			// "声明 tool_calls 的 assistant 紧跟它自己的结果"在两种拆分顺序下都成立。
			//
			// 谁会产出这个顺序：部分 OpenAI 兼容 agent 客户端的回放顺序（正文与
			// 调用声明拆成两条独立 assistant）。
			//
			// 条件同样从严：只认**字符串正文**（数组正文可能含多模态块，拼接会丢结构，
			// 交给 repackToolResultBlocks 原样处理）；上一条必须自身无正文。
			if _, hasCalls := msg["tool_calls"]; !hasCalls {
				if txt, ok := msg["content"].(string); ok && txt != "" {
					if prev, ok := out[len(out)-1].(map[string]any); ok {
						if prevRole, _ := prev["role"].(string); prevRole == "assistant" {
							if prevCalls, ok := prev["tool_calls"].([]any); ok && len(prevCalls) > 0 && emptyContent(prev["content"]) {
								prev["content"] = txt
								mergeReasoningContent(prev, msg)
								changed = true
								continue // 正文已折进上一条
							}
						}
					}
				}
			}
		}
		out = append(out, m)
	}
	if !changed {
		return messages, false
	}
	return out, true
}

// mergeReasoningContent 把 src 的 reasoning_content 并入 dst（两边都有则换行拼接）。
// deepseek 多轮要求 assistant 带思维链回填，合并时丢弃会换一个错误。
func mergeReasoningContent(dst, src map[string]any) {
	rc, _ := src["reasoning_content"].(string)
	if rc == "" {
		return
	}
	if prevRC, _ := dst["reasoning_content"].(string); prevRC != "" {
		dst["reasoning_content"] = prevRC + "\n" + rc
		return
	}
	dst["reasoning_content"] = rc
}

// emptyContent content 是否为空（缺失 / nil / 空串 / 空数组）。
// 空数组也必须算空：有客户端把"没有正文"发成 `content: []` 而不是 null，此前按
// "非字符串一律非空"处理 → 背靠背的两条 assistant(tool_calls) 不合并 → 上游
// deepseek 系判 11148（正是 mergeAdjacentToolCalls 要挡的形态，条件漏了 []）。
// 非空数组仍视为有内容：宁可漏合并，也不丢内容。
func emptyContent(v any) bool {
	switch c := v.(type) {
	case nil:
		return true
	case string:
		return c == ""
	case []any:
		return len(c) == 0
	}
	return false
}
