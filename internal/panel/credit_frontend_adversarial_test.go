package panel

// T5 前端对抗测试：node + vm 切片（仿 frontend_test.go 的 TestAppJSCreditHistoryFormatting）。
// 脏数据 / XSS / 501 分支不得抛异常、不得输出未转义 HTML。只新增测试，不改业务代码。

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// bndCreditNode 把脚本写到临时文件并用 node 执行（工作目录 = 包目录，读 app.js），
// 返回去掉首尾空白的 stdout；无 node 时跳过。
func bndCreditNode(t *testing.T, script string) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; credit history adversarial test skipped")
	}
	f, err := os.CreateTemp(t.TempDir(), "credit-adv-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("credit history adversarial node test failed: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// 脏数据 / XSS / 不可用分支。返回 JSON：
//
//	*Ok = true 表示未抛异常；其余字段是渲染结果，供 Go 侧断言转义与文案。
const bndCreditAdversarialScript = `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const escStart = src.indexOf('function esc(');
const escEnd = src.indexOf('function ago(');
const start = src.indexOf('function creditNum');
const end = src.indexOf('function creditLimitValue');
if ([escStart, escEnd, start, end].some(v => v < 0)) throw new Error('credit history helpers not found');
const els = {};
const $ = id => (els[id] || (els[id] = { innerHTML: '', textContent: '' }));
const ctx = { Date, Number, String, Math, RegExp, isNaN, $ };
vm.createContext(ctx);
vm.runInContext(src.slice(escStart, escEnd) + src.slice(start, end) +
  '\nthis.renderCreditHistory=renderCreditHistory;', ctx);
const t = new Date(2026, 8, 28, 14, 5, 6).toISOString();
const out = {};
const run = (key, fn) => { try { fn(); out[key] = true; } catch (e) { out[key] = String((e && e.stack) || e); } };

run('nullEntriesOk', () => ctx.renderCreditHistory({ entries: null }));
out.nullEntriesNote = els.creditNote.textContent;
out.nullEntriesBody = els.creditBody.innerHTML;

run('missingFieldsOk', () => ctx.renderCreditHistory({ entries: [{}] }));
out.missingFieldsBody = els.creditBody.innerHTML;

run('badDeltaOk', () => ctx.renderCreditHistory({ entries: [
  { time: t, uid: 'u1', delta: '50', before: '0', after: '50' },
  { time: t, uid: 'u2', delta: 'abc', before: 0, after: 0 },
  { time: t, uid: 'u3', delta: NaN, before: 0, after: 0 },
] }));
out.badDeltaBody = els.creditBody.innerHTML;

const evilAccount = '<script>alert(1)</script>"\' onmouseover="x';
run('xssOk', () => ctx.renderCreditHistory({ entries: [
  { time: t, uid: '<img src=x onerror=alert(1)>', account: evilAccount, delta: 1, before: 0, after: 1 },
] }));
out.xssBody = els.creditBody.innerHTML;

run('unavailableOk', () => ctx.renderCreditHistory(null));
out.unavailableNote = els.creditNote.textContent;
out.unavailableBody = els.creditBody.innerHTML;

process.stdout.write(JSON.stringify(out));`

func TestBndCreditFrontendAdversarial(t *testing.T) {
	out := bndCreditNode(t, bndCreditAdversarialScript)
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("node 输出不是 JSON: %v\n%s", err, out)
	}
	str := func(k string) string {
		s, _ := got[k].(string)
		return s
	}
	ok := func(k string) {
		t.Helper()
		if got[k] != true {
			t.Fatalf("%s: 渲染抛异常或未执行: %v", k, got[k])
		}
	}

	// entries=null：不抛异常，回「暂无」而不是「不可用」。
	ok("nullEntriesOk")
	if note := str("nullEntriesNote"); note != "暂无积分变动记录" {
		t.Fatalf("entries=null 的提示应为暂无，得到 %q", note)
	}
	if body := str("nullEntriesBody"); !strings.Contains(body, "暂无积分变动记录") {
		t.Fatalf("entries=null 的表格应为暂无，得到 %s", body)
	}

	// 字段全缺：不抛异常，以 — / 0 兜底。
	ok("missingFieldsOk")
	if body := str("missingFieldsBody"); !strings.Contains(body, "余额 0 → 0") || !strings.Contains(body, "—") {
		t.Fatalf("缺字段行应以 — / 0 兜底，得到 %s", body)
	}

	// delta 为字符串 / 非法字符串 / NaN：不抛异常，不渲染出 NaN。
	ok("badDeltaOk")
	badDelta := str("badDeltaBody")
	if !strings.Contains(badDelta, "+50") {
		t.Fatalf("字符串 50 应按数字渲染为 +50，得到 %s", badDelta)
	}
	if strings.Contains(badDelta, "NaN") {
		t.Fatalf("delta 非法时不应渲染出 NaN，得到 %s", badDelta)
	}
	if n := strings.Count(badDelta, "<tr>"); n != 3 {
		t.Fatalf("应渲染 3 行，得到 %d: %s", n, badDelta)
	}

	// account / uid 含 <script>、<img>、引号：必须全部转义。
	ok("xssOk")
	xss := str("xssBody")
	for _, raw := range []string{"<script>", "<img", `onmouseover="x"`} {
		if strings.Contains(xss, raw) {
			t.Fatalf("未转义的 %q 出现在输出中: %s", raw, xss)
		}
	}
	for _, enc := range []string{"&lt;script&gt;", "&quot;", "&#39;"} {
		if !strings.Contains(xss, enc) {
			t.Fatalf("输出缺少转义结果 %q: %s", enc, xss)
		}
	}

	// 不可用分支（catch 到 null）：文案必须是「积分历史不可用」，不能混成「暂无」。
	ok("unavailableOk")
	if note := str("unavailableNote"); note != "积分历史不可用" {
		t.Fatalf("不可用提示应为「积分历史不可用」，得到 %q", note)
	}
	if body := str("unavailableBody"); !strings.Contains(body, "积分历史不可用") || strings.Contains(body, "暂无") {
		t.Fatalf("不可用表格文案不符: %s", body)
	}
}

// time 非法字符串：不抛异常（由 node 退出码保证），并应以 — 兜底显示。
const bndCreditInvalidTimeScript = `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function creditNum');
const end = src.indexOf('function creditLimitValue');
if (start < 0 || end < 0) throw new Error('credit history helpers not found');
const ctx = { Date, Number, String, Math, RegExp, isNaN };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.creditEntryText=creditEntryText;', ctx);
process.stdout.write(JSON.stringify({
  bad: ctx.creditEntryText({ time: 'not-a-date', uid: 'u1', delta: 1, before: 0, after: 1 }),
  empty: ctx.creditEntryText({ time: '', uid: 'u1', delta: 1, before: 0, after: 1 }),
}));`

func TestBndCreditFrontendInvalidTimeFallback(t *testing.T) {
	out := bndCreditNode(t, bndCreditInvalidTimeScript)
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("node 输出不是 JSON: %v\n%s", err, out)
	}
	if !strings.HasPrefix(got["empty"], "—") {
		t.Fatalf("空 time 应以 — 兜底，得到 %q", got["empty"])
	}
	if !strings.HasPrefix(got["bad"], "—") {
		t.Fatalf("非法 time 应以 — 兜底（不抛异常、不显示 Invalid Date），得到 %q", got["bad"])
	}
}
