// cache_key.go 注入上游 prompt_cache_key 字段（P0 费用优化）。
//
// 逆向实测（buddy-adapter.ts:706-714）：上游服务端支持 prompt_cache_key，
// 同一段 8k token 前缀：
//   - 不带 → prompt_cache_hit_tokens=0, credit≈0.34
//   - 带   → prompt_cache_hit_tokens=7808, credit≈0.02（费用降 ~17×）
//
// 网关在此为每个出站 chat 请求注入一个稳定、按账号隔离的 cache key，
// 让同一客户端对同一账号的连续请求复用上游前缀缓存。
package upstream

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
)

// InjectPromptCacheKey 在已改写的出站 body 上注入 prompt_cache_key 字段。
//
// 优先级：
//  1. body 已带 prompt_cache_key → 原值保留（客户端自知复用哪个键）
//  2. body 已带 conversation_id / conversationId → 用它做会话哈希源
//  3. 都没有 → 用入站 conversationID 参数（来自 X-Conversation-ID 头解析）
//
// 安全约束——按账号隔离：
//   - 生成键格式 = UUID v4 形态 36 字符（见 buildCacheKey），与官方 X-Conversation-ID 同形
//   - uid 是哈希输入的一部分，跨账号绝不相同 → 跨账号缓存键绝不碰撞
//   - 跨账号复用同一 cache key 会让上游命中错账号的前缀缓存、泄露对方对话，故 uid 是硬隔离因子
//   - 键里不再出现项目缩写或 uid 明文段（issue #239：那两处都是可判别指纹）
//
// 入参 uid 为账号 UID（空则用 "-"，但仍会注入键；调用方应保证传真实 UID）。
// 入参 conversationID 为网关解析出的会话标识（body 里没有 conversation_id 时用它做哈希源）。
// 两源都空时 convHex 为定值（每次新会话不复用前缀，但仍保留账号隔离段）。
// body 不可解析时原样返回（与 prepareBody 语义一致：坏 body 不二次错误化）。
func InjectPromptCacheKey(body []byte, uid, conversationID string) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	// 优先级 1：客户端已显式带 key → 绝不覆盖。
	if existing, ok := obj["prompt_cache_key"].(string); ok && existing != "" {
		return body
	}
	// 优先级 2：body 里的 conversation_id / conversationId 做会话哈希源。
	conv := conversationID
	if v := strField(obj, "conversation_id"); v != "" {
		conv = v
	} else if v := strField(obj, "conversationId"); v != "" {
		conv = v
	}
	key := buildCacheKey(uid, conv)
	obj["prompt_cache_key"] = key
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// buildCacheKey 生成与官方客户端同形的会话标识：UUID v4 形态的 36 字符。
//
// 为什么不再用 `wb2a-<uid8>-<convHex>`（issue #239 / #51）：旧格式长 48 字符、
// 自带项目缩写、并把账号 UID 前 8 位**明文**写进请求头与 body——三处都可与官方值
// 区分，等于自证网关身份。官方出站 X-Conversation-ID 实测恒为 36 字符带横线 UUID。
//
// 语义不变：
//   - uid 仍参与哈希输入 → 跨账号绝不碰撞（隔离靠哈希输入，不再靠明文 uid 段）；
//   - 同 (uid, conversation) 恒得同值 → 会话级前缀缓存槽语义与旧实现一致；
//   - 空会话源时参数固化，仍不复用其他会话的槽。
func buildCacheKey(uid, conversation string) string {
	return buildStableUUID(uid, conversation, "")
}

// buildStableUUID 由 (uid, conversation, purpose) 稳定派生一个 UUID v4 形态的 36 字符值。
//
// purpose 是**域分隔段**：同一 (账号, 会话) 需要多个互不相同又各自稳定的标识时用它区分。
// purpose 为空时输入与历史 buildCacheKey 的语义一致。
//
// 版本/变体位按 RFC 4122 置位（第 7 字节 0x40、第 9 字节 0x80），使形态与真 UUID v4
// 完全一致——否则 `4xxx`/`89ab` 位置出现别的值，反而是另一种可判别特征。
func buildStableUUID(uid, conversation, purpose string) string {
	in := uid + "|" + conversation
	if purpose != "" {
		in += "|" + purpose
	}
	sum := sha256.Sum256([]byte(in))
	var u [16]byte
	copy(u[:], sum[:16])
	u[6] = (u[6] & 0x0f) | 0x40 // version 4
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16])
}

// strField 从 map 取 string 字段，非 string 或空串返回 ""。
func strField(obj map[string]any, key string) string {
	v, ok := obj[key].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}
