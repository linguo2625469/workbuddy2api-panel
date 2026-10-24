package session

import (
	"regexp"
	"strings"
	"testing"
)

// TestNewConversationID 形态钉住：v4 UUID（36 字符带横线），与官方客户端的
// X-Conversation-ID 同形。旧实现用 `wb2api-<毫秒>` 之类带项目缩写的形式——
// 那是最直白的一类自证标识（issue #239）。
func TestNewConversationID(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := NewConversationID()
		if len(id) != 36 {
			t.Fatalf("长度应为 36，实际 %d: %q", len(id), id)
		}
		if !re.MatchString(id) {
			t.Fatalf("不是 v4 UUID 形态: %q", id)
		}
		if strings.Contains(id, "wb2api") || strings.Contains(id, "wb2a") {
			t.Fatalf("不得含项目缩写: %q", id)
		}
		if seen[id] {
			t.Fatalf("重复 ID: %q", id)
		}
		seen[id] = true
	}
}
