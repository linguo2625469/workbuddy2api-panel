package session

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/redisstore"
)

// countingStore 记录镜像调用次数的假 Store（不联网）。
type countingStore struct {
	redisstore.Noop
	mu       sync.Mutex
	setBinds int
	delBinds int
	binds    map[string]string
}

func newCountingStore() *countingStore {
	return &countingStore{binds: map[string]string{}}
}

func (c *countingStore) SetBind(key, uid string, ttl time.Duration) {
	c.mu.Lock()
	c.setBinds++
	c.binds[key] = uid
	c.mu.Unlock()
}
func (c *countingStore) DelBind(key string) {
	c.mu.Lock()
	c.delBinds++
	delete(c.binds, key)
	c.mu.Unlock()
}
func (c *countingStore) LoadBinds() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]string{}
	for k, v := range c.binds {
		out[k] = v
	}
	return out
}

func routerWith(store redisstore.Store, avail []string, ttl time.Duration) *Router {
	return New(Config{
		TTL:       ttl,
		Store:     store,
		Available: func() []string { return avail },
	})
}

func TestWeightedVirtualCandidatesBiasNewSessions(t *testing.T) {
	st := newCountingStore()
	// 一个普通账号 1 个虚拟实例，一个快过期账号 3 个虚拟实例。
	r := routerWith(st, []string{"regular", "expiring", "expiring", "expiring"}, time.Minute)

	// 先让每个真实账号都已有绑定，确保后续新会话进入全池哈希阶段，而不是
	// 被“未绑定账号优先”规则逐个填满。这样可以直接验证虚拟实例权重。
	r.Bind("warm-regular", "regular")
	r.Bind("warm-expiring", "expiring")

	counts := map[string]int{}
	const n = 4000
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("session-%d", i)
		uid, ok := r.Resolve(key)
		if !ok {
			t.Fatal("Resolve returned !ok")
		}
		counts[uid]++
	}

	share := float64(counts["expiring"]) / float64(n)
	if share < 0.70 || share > 0.80 {
		t.Fatalf("weighted expiring share=%.3f counts=%v, want 70%%-80%%", share, counts)
	}
}

func TestSameKeySameAccount(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1", "a2"}, time.Minute)
	u1, ok1 := r.Resolve("c1")
	u2, ok2 := r.Resolve("c1")
	if !ok1 || !ok2 || u1 != u2 {
		t.Fatalf("same key should map to same account: %s vs %s", u1, u2)
	}
	if r.Count() != 1 {
		t.Errorf("count=%d want 1", r.Count())
	}
}

func TestTTLExpiryReassigns(t *testing.T) {
	st := newCountingStore()
	r := routerWith(st, []string{"a1", "a2"}, 10*time.Millisecond)
	u1, _ := r.Resolve("c1")
	time.Sleep(20 * time.Millisecond)
	u2, ok := r.Resolve("c1")
	if !ok {
		t.Fatal("resolve after expiry should still succeed")
	}
	// 过期后可重新分配（可能巧合同号，但至少返回有效账号）。
	_ = u1
	_ = u2
	if r.Count() != 1 {
		t.Errorf("count=%d want 1 (reassigned, not duplicated)", r.Count())
	}
}

func TestBoundAccountCooldownReassigns(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1"}, time.Minute)
	u1, _ := r.Resolve("c1")
	if u1 != "a1" {
		t.Fatalf("initial bind=%s want a1", u1)
	}
	// a1 冷却 → 可用列表只剩 a2 → 重新分配必须换到 a2。
	r.cfg.Available = func() []string { return []string{"a2"} }
	u2, ok := r.Resolve("c1")
	if !ok {
		t.Fatal("resolve should succeed with fallback account")
	}
	if u2 == u1 {
		t.Fatalf("bound account %s cooled but still assigned", u1)
	}
	if u2 != "a2" {
		t.Fatalf("reassigned to %s want a2", u2)
	}
}

func TestNoSessionKeyPassthrough(t *testing.T) {
	// ExtractKey 找不到任何会话键 → 空串（调用方据空串走普通 Pick；router 不会被调用）。
	got := ExtractKey([]byte(`{"model":"x","messages":[]}`))
	if got != "" {
		t.Errorf("ExtractKey should return empty, got %q", got)
	}
}

func TestExtractKeyPriority(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"metadata":{"conversation_id":"mc","user_id":"mu"},"conversation_id":"top"}`, "mc"}, // metadata.conversation_id 优先
		{`{"conversation_id":"top"}`, "top"},                                                   // 顶层 conversation_id
		{`{"metadata":{"user_id":"mu"}}`, ""},                                                  // user_id 已剔除粘性键（粒度过粗），回落轮换
		{`{"metadata":{"conversation_id":123}}`, ""},                                           // 非字符串 → 空
		{`not-json`, ""}, // 非法 JSON → 空
		// issue #35：客户端实际发 camelCase conversationId，ExtractKey 必须识别。
		{`{"conversationId":"abc"}`, "abc"},                                            // 顶层 camelCase
		{`{"metadata":{"conversationId":"abc"}}`, "abc"},                               // metadata.camelCase
		{`{"metadata":{"conversation_id":"snake","conversationId":"camel"}}`, "snake"}, // snake 优先于 camel
		{`{"conversation_id":"snake","conversationId":"camel"}`, "snake"},              // 顶层 snake 优先于 camel
		{`{"conversationId":123}`, ""},                                                 // 数字 conversationId → 空
		{`{"metadata":{"conversationId":456}}`, ""},                                    // metadata 数字 conversationId → 空
		{`{"metadata":{"conversationId":"abc","user_id":"mu"}}`, "abc"},                // camel conversationId 优先于 user_id
	}
	for _, c := range cases {
		if got := ExtractKey([]byte(c.body)); got != c.want {
			t.Errorf("ExtractKey(%s)=%q want %q", c.body, got, c.want)
		}
	}
}

func TestConcurrentSameKeyAssignsOnce(t *testing.T) {
	avail := []string{"a1", "a2", "a3", "a4", "a5"}
	r := routerWith(newCountingStore(), avail, time.Minute)

	const N = 100
	uids := make([]string, N)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			u, ok := r.Resolve("same-key")
			if ok {
				uids[idx] = u
			}
		}(i)
	}
	wg.Wait()

	// 所有 goroutine 必须拿到同一个账号（写锁 re-check 防重复分配）。
	first := ""
	for _, u := range uids {
		if u == "" {
			t.Fatal("some goroutine failed to resolve")
		}
		if first == "" {
			first = u
		}
		if u != first {
			t.Fatalf("concurrent resolve assigned different accounts: %s vs %s", first, u)
		}
	}
	if r.Count() != 1 {
		t.Errorf("count=%d want 1 (single binding)", r.Count())
	}
}

// boundUID 直接读绑定 uid（不触发 Resolve 的重分配），供 Bind 系列测试断言用（包内私有 helper）。
func (r *Router) boundUID(key string) (string, bool) {
	r.mu.RLock()
	e, ok := r.entries[key]
	r.mu.RUnlock()
	return e.uid, ok
}

func TestBindOverridesAndMirrors(t *testing.T) {
	// Bind 幂等覆盖旧值，并异步镜像 SetBind。
	st := newCountingStore()
	r := routerWith(st, []string{"a1", "a2"}, time.Minute)
	r.Bind("c1", "a1")
	if u, ok := r.boundUID("c1"); !ok || u != "a1" {
		t.Fatalf("bind c1->a1 then bound=%s ok=%v", u, ok)
	}
	// 覆盖到 a2
	r.Bind("c1", "a2")
	if u, _ := r.boundUID("c1"); u != "a2" {
		t.Fatalf("bind override should map c1->a2, got %s", u)
	}
	if r.Count() != 1 {
		t.Errorf("bind override must not duplicate entries, count=%d", r.Count())
	}
	st.mu.Lock()
	n := st.setBinds
	binds := map[string]string{}
	for k, v := range st.binds {
		binds[k] = v
	}
	st.mu.Unlock()
	if n != 2 {
		t.Errorf("SetBind mirror count=%d want 2", n)
	}
	if binds["c1"] != "a2" {
		t.Errorf("mirrored bind should be a2, got %s", binds["c1"])
	}
}

func TestBindIgnoresEmptyKey(t *testing.T) {
	st := newCountingStore()
	r := routerWith(st, []string{"a1"}, time.Minute)
	r.Bind("", "a1")
	r.Bind("c1", "")
	if r.Count() != 0 {
		t.Errorf("Bind with empty key/uid must be no-op, count=%d", r.Count())
	}
	st.mu.Lock()
	n := st.setBinds
	st.mu.Unlock()
	if n != 0 {
		t.Errorf("empty-key Bind must not mirror, setBinds=%d", n)
	}
}

func TestBindThenUnbindLifecycle(t *testing.T) {
	st := newCountingStore()
	r := routerWith(st, []string{"a1"}, time.Minute)
	r.Bind("c1", "a1")
	if !r.Unbind("c1") {
		t.Fatal("Unbind should report found")
	}
	if r.Count() != 0 {
		t.Errorf("count after unbind=%d want 0", r.Count())
	}
	st.mu.Lock()
	del := st.delBinds
	st.mu.Unlock()
	if del != 1 {
		t.Errorf("DelBind mirror count=%d want 1", del)
	}
}

func TestRedisMirrorSetBindCount(t *testing.T) {
	st := newCountingStore()
	r := routerWith(st, []string{"a1", "a2"}, time.Minute)
	r.Resolve("c1")
	r.Resolve("c1") // 快路径 touch → 又镜像一次
	if st.setBinds < 1 {
		t.Errorf("SetBind mirror count=%d want >=1", st.setBinds)
	}
	r.Unbind("c1")
	if st.delBinds != 1 {
		t.Errorf("DelBind mirror count=%d want 1", st.delBinds)
	}
}

func TestGCCleansExpired(t *testing.T) {
	st := newCountingStore()
	r := routerWith(st, []string{"a1"}, 10*time.Millisecond)
	r.Resolve("c1")
	r.Resolve("c2")
	time.Sleep(20 * time.Millisecond)
	removed := r.gcOnce(time.Now())
	if removed != 2 {
		t.Errorf("gc removed=%d want 2", removed)
	}
	if r.Count() != 0 {
		t.Errorf("count after gc=%d want 0", r.Count())
	}
}

func TestLoadFromStoreRestores(t *testing.T) {
	st := newCountingStore()
	st.binds["c1"] = "a1"
	st.binds["c2"] = "a2"
	r := routerWith(st, []string{"a1", "a2"}, time.Minute)
	r.LoadFromStore()
	if r.Count() != 2 {
		t.Fatalf("restored count=%d want 2", r.Count())
	}
	u, ok := r.Resolve("c1")
	if !ok || u != "a1" {
		t.Errorf("restored c1 -> %s want a1", u)
	}
}

// TestExtractKeyDerivedFromContent 客户端不发会话 id 时回退到内容派生键：
// 同一对话多轮（历史追加）→ 键稳定不变；不同对话 → 键不同。
func TestExtractKeyDerivedFromContent(t *testing.T) {
	// 第一轮
	turn1 := `{"model":"glm-5.3","messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"帮我写个排序算法"}]}`
	// 第二轮：历史追加了 assistant 与新的 user（system 与首条 user 不变）
	turn2 := `{"model":"glm-5.3","messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"帮我写个排序算法"},{"role":"assistant","content":"好的"},{"role":"user","content":"换成快排"}]}`
	k1, k2 := ExtractKey([]byte(turn1)), ExtractKey([]byte(turn2))
	if k1 == "" {
		t.Fatal("derived key should not be empty when messages present")
	}
	if k1 != k2 {
		t.Errorf("derived key must be stable across turns: turn1=%q turn2=%q", k1, k2)
	}
	if !strings.HasPrefix(k1, "d-") {
		t.Errorf("derived key should carry prefix d-: %q", k1)
	}
	// 不同对话（首条 user 不同）→ 不同键
	other := `{"model":"glm-5.3","messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"翻译这段话"}]}`
	if ExtractKey([]byte(other)) == k1 {
		t.Error("different first user message must yield a different derived key")
	}
	// 显式 id 优先于派生键
	withID := `{"conversation_id":"my-session","messages":[{"role":"user","content":"帮我写个排序算法"}]}`
	if got := ExtractKey([]byte(withID)); got != "my-session" {
		t.Errorf("explicit id must win over derived key, got %q", got)
	}
	// 无 messages / 纯无文本内容 → 空（退回普通轮换，不误粘）
	if got := ExtractKey([]byte(`{"model":"x"}`)); got != "" {
		t.Errorf("no messages should yield empty key, got %q", got)
	}
	if got := ExtractKey([]byte(`{"messages":[{"role":"user","content":[]}]}`)); got != "" {
		t.Errorf("text-less content should yield empty key, got %q", got)
	}
}

// TestExtractKeyMultimodalContent 多模态 content 数组取文本部分派生。
func TestExtractKeyMultimodalContent(t *testing.T) {
	body := `{"messages":[{"role":"user","content":[{"type":"text","text":"看图说话"},{"type":"image_url","image_url":{"url":"http://x/y.png"}}]}]}`
	k := ExtractKey([]byte(body))
	if k == "" || !strings.HasPrefix(k, "d-") {
		t.Fatalf("multimodal text should derive a key, got %q", k)
	}
	// 同一文本（图片不同）→ 同键（图片不参与派生，避免签名 URL 变化破坏粘性）
	body2 := `{"messages":[{"role":"user","content":[{"type":"text","text":"看图说话"},{"type":"image_url","image_url":{"url":"http://x/z.png"}}]}]}`
	if ExtractKey([]byte(body2)) != k {
		t.Error("image url changes must not break derived key stability")
	}
	// 纯图片首条 user（无 text part）也应派生非空键（首图会话粘性盲区修复；
	// 图片只入类型占位，同图重发/换 URL 均同键）。
	imgOnly := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"http://x/y.png"}}]}]}`
	k2 := ExtractKey([]byte(imgOnly))
	if k2 == "" || !strings.HasPrefix(k2, "d-") {
		t.Fatalf("pure-image first user should derive a key, got %q", k2)
	}
	imgOnly2 := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"http://x/z.png"}}]}]}`
	if ExtractKey([]byte(imgOnly2)) != k2 {
		t.Error("pure-image url changes must keep same derived key")
	}
}

func TestLookupForModelOnlyReusesValidBinding(t *testing.T) {
	available := []string{"a", "b"}
	store := newCountingStore()
	r := New(Config{TTL: time.Minute, Store: store, AvailableForModel: func(model string) []string { return available }})
	if _, ok := r.LookupForModel("new", "m"); ok || r.Count() != 0 {
		t.Fatal("lookup allocated a new binding")
	}
	r.Bind("key", "a")
	if uid, ok := r.LookupForModel("key", "m"); !ok || uid != "a" {
		t.Fatalf("valid lookup=%s %v", uid, ok)
	}
	available = []string{"b"}
	if _, ok := r.LookupForModel("key", "m"); ok {
		t.Fatal("unavailable binding reused")
	}
	available = []string{"a", "b"}
	r.mu.Lock()
	r.entries["key"] = entry{uid: "a", lastActive: time.Now().Add(-2 * time.Minute)}
	r.mu.Unlock()
	if _, ok := r.LookupForModel("key", "m"); ok {
		t.Fatal("expired binding reused")
	}
	if r.Count() != 1 {
		t.Fatal("lookup allocated a replacement")
	}
}
