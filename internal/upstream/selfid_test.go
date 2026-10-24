package upstream

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoSelfIdentifyingPrefixInOutbound 钉住「上行请求体/请求头不得含项目自证前缀」
// 这条约束（issue #239）。
//
// 背景：项目缩写曾作为会话/请求 ID 直接进入上游上报体（conversationId、
// parentConversationId、traceId、rootRequestId、requestId、prompt_cache_key）
// 与请求头。官方客户端的同位置恒为 UUID v4（36 字符带横线）或 32 hex——
// 带项目缩写的值能被一眼识别为「非官方客户端」，是可判别的自证指纹。
//
// 本测试扫描 internal/ 下的 Go 源码，断言不存在 `wb2api-` / `wb2a-` 字面量
// 出现在**代码**里（注释与文档中的历史说明被排除，因为它们记录的是修复背景）。
// 后续新增调用点若图省事写回 `fmt.Sprintf("wb2api-%d", ...)`，本测试立刻失败。
func TestNoSelfIdentifyingPrefixInOutbound(t *testing.T) {
	// 允许出现在注释里的说明性文本（修复背景记录），但代码里的字面量一律禁止。
	litRe := regexp.MustCompile(`"wb2a(?:pi)?[-_][^"]*"`)

	roots := []string{"..", "."}
	seen := map[string]bool{}
	for _, root := range roots {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			// 只扫生产代码：测试文件里的字面量是夹具/断言材料，不进上行路径。
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}
			abs, _ := filepath.Abs(path)
			if seen[abs] {
				return nil
			}
			seen[abs] = true

			raw, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			for i, line := range strings.Split(string(raw), "\n") {
				trimmed := strings.TrimSpace(line)
				// 注释行（// 开头）允许出现历史说明。
				if strings.HasPrefix(trimmed, "//") {
					continue
				}
				if m := litRe.FindString(trimmed); m != "" {
					t.Errorf("%s:%d 上行代码里出现项目自证前缀字面量 %s", path, i+1, m)
				}
			}
			return nil
		})
	}
}
