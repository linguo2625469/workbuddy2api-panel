package panel

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// 本文件覆盖「官方补偿」卡片（issue #189）的前端纯函数与渲染。
//
// 为什么要有：compPending 是「能不能领」的唯一判据，compDeadlineText 决定用户
// 会不会错过截止日，renderCompensation 决定**失败会不会显形**——三者都只在浏览器里
// 生效，Go 侧不执行 JS，编译与后端测试对它们完全失明。判据写反或让错误静默隐藏，
// 都不报错、只能肉眼看，故在此用 node harness 钉死。

func TestAppJSCompPendingGate(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; compensation gate test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function compPending');
const end = src.indexOf('function compDeadlineText');
if (start < 0 || end < 0 || end < start) throw new Error('compensation region not found');
const ctx = { Array, Object, Number, String };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) +
  '\nthis.compPending = compPending; this.compTotalPending = compTotalPending; this.compErrored = compErrored;', ctx);

const rows = [
  { uid: 'a', status: { active: true,  claimed: false, credit_num: 500 } }, // 待领
  { uid: 'b', status: { active: true,  claimed: true,  credit_num: 500 } }, // 已领
  { uid: 'c', status: { active: false, claimed: false, credit_num: 0   } }, // 无活动
  { uid: 'd', status: { active: true,  claimed: false, credit_num: 0   } }, // 额度为 0
  // ↓ 下面两行是**判别力用例**：状态本身完全「待领」，只有 skip / error 能排除它们。
  // 若样本里没有 status（或 status 非 pending），r.status 子句会先行短路排除，
  // 于是删掉 !r.skip / !r.error 本用例仍然通过——等于没测到那两条判据。
  { uid: 'e', skip: 'enterprise', skip_msg: '企业版',
    status: { active: true, claimed: false, credit_num: 500 } },
  { uid: 'f', error: 'boom',
    status: { active: true, claimed: false, credit_num: 500 } },
  null,
];process.stdout.write(JSON.stringify({
  uids: ctx.compPending(rows).map(r => r.uid),
  total: ctx.compTotalPending(rows),
  errored: ctx.compErrored(rows).map(r => r.uid),
  emptyOk: ctx.compPending(null).length === 0 && ctx.compPending(undefined).length === 0,
  dirtyOk: ctx.compPending([null, undefined]).length === 0,
}));`
	f, err := os.CreateTemp(t.TempDir(), "compgate-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("compensation gate node test failed: %v\n%s", err, out)
	}
	want := `{"uids":["a"],"total":500,"errored":["f"],"emptyOk":true,"dirtyOk":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("compensation gate=%s\nwant %s", strings.TrimSpace(string(out)), want)
	}
}

// TestAppJSCompGateMutationDiscriminates 反向证明：把 !r.skip / !r.error 两条判据
// 从 app.js 副本里删掉，本用例必须 FAIL。
//
// 这类「变异测试」是本文件存在的主要理由——正向断言全绿并不代表测到了东西，
// 若样本造得不好（比如被测行没有 status 字段），判据会被 r.status 短路吃掉，
// 删除判据照样全绿。review 时请连同 app.js 副本一起看。
func TestAppJSCompGateMutationDiscriminates(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; compensation mutation test skipped")
	}
	mutants := map[string]struct{ from, to string }{
		"drop !r.skip":      {"!r.skip && ", ""},
		"drop !r.error":     {"!r.error && ", ""},
		"drop r.active":     {"r.status.active && ", ""},
		"drop !claimed":     {"!r.status.claimed && ", ""},
		"drop credit>0":     {"(r.status.credit_num || 0) > 0", "true"},
		"drop whole status": {"r.status &&", "true &&"},
	}
	for name, m := range mutants {
		t.Run(name, func(t *testing.T) {
			orig, err := os.ReadFile("app.js")
			if err != nil {
				t.Fatal(err)
			}
			mutated := strings.Replace(string(orig), m.from, m.to, 1)
			if mutated == string(orig) {
				t.Fatalf("变异未生效：app.js 里找不到 %q（判据已改写？）", m.from)
			}
			tmp := t.TempDir()
			mutFile := tmp + "/app.js"
			if err := os.WriteFile(mutFile, []byte(mutated), 0o600); err != nil {
				t.Fatal(err)
			}
			script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function compPending');
const end = src.indexOf('function compDeadlineText');
const ctx = { Array, Object, Number, String };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.compPending = compPending;', ctx);
const rows = [
  { uid: 'a', status: { active: true,  claimed: false, credit_num: 500 } }, // 唯一应保留
  { uid: 'g', status: { active: true,  claimed: true,  credit_num: 500 } }, // 删 !claimed 会漏进来
  { uid: 'h', status: { active: true,  claimed: false, credit_num: 0   } }, // 删 credit>0 会漏进来
  { uid: 'i', status: { active: false, claimed: false, credit_num: 500 } }, // 删 active 会漏进来
  { uid: 'e', skip: 'enterprise', status: { active: true, claimed: false, credit_num: 500 } },
  { uid: 'f', error: 'boom',      status: { active: true, claimed: false, credit_num: 500 } },
  // 完全没有 status 的脏行：r.status 守卫挡住它（否则 r.status.active 抛 TypeError，
  // node 非零退出也算「变异被抓到」）。这条判据测的是崩溃安全而非过滤结果。
  { uid: 'k', nickname: '脏数据' },
];
process.stdout.write(JSON.stringify(ctx.compPending(rows).map(r => r.uid)));`
			sf := tmp + "/m.cjs"
			if err := os.WriteFile(sf, []byte(script), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command(node, sf, mutFile).CombinedOutput()
			// 变异被「抓到」有两种形态：① 跑通了但输出变了（判据被删 → 多出/少了
			// 账号）；② 直接崩了（如删掉 r.status 空值守卫后 TypeError）。两者都算
			// 有判别力；只有「跑通且输出与基线完全一致」才是假通过。
			if err != nil {
				return // 崩溃 = 检出
			}
			if got := strings.TrimSpace(string(out)); got == `["a"]` {
				t.Fatalf("变异 %s 后 compPending 结果不变 = 本用例对该判据无判别力（假通过）", name)
			}
		})
	}
}

func TestAppJSCompDeadlineText(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; compensation deadline test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function compDeadlineText');
const end = src.indexOf('function renderCompensation');
if (start < 0 || end < 0 || end < start) throw new Error('deadline region not found');
const ctx = { Date, Math, Number, String, isFinite };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.f = compDeadlineText;', ctx);

// 上游 end_time 是不带时区的 "YYYY-MM-DD HH:mm:ss"，按本地时区格式化造输入，
// 这样用例在任何 TZ 下都自洽（写死日期会在换季/换区后自己失效）。
const p = n => String(n).padStart(2, '0');
const mkEnd = ms => { const d = new Date(ms);
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' +
         p(d.getHours()) + ':' + p(d.getMinutes()) + ':' + p(d.getSeconds()); };
const base = new Date(2026, 9, 22, 23, 59, 59).getTime();   // 2026-10-22 23:59:59 本地时
const H = 3600000, D = 86400000;
process.stdout.write(JSON.stringify({
  six:   ctx.f(mkEnd(base), base - 6 * H),          // 只剩 6 小时 → 临期
  three: ctx.f(mkEnd(base), base - 3 * D),          // 还剩 3 天 → 不告警
  past:  ctx.f(mkEnd(base), base + H),              // 已过截止
  empty: ctx.f('', base),                            // 上游没给 end_time
  junk:  ctx.f('not-a-date', base),                  // 解析失败不得抛
  undef: ctx.f(null, base),
}));`
	f, err := os.CreateTemp(t.TempDir(), "compdl-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("compensation deadline node test failed: %v\n%s", err, out)
	}
	got := strings.TrimSpace(string(out))
	for _, must := range []string{
		`"text":"剩 6 小时","urgent":true`,      // ≤48h 必须标临期
		`"text":"剩 3 天 0 小时","urgent":false`, // 3 天不算临期
		`"text":"已截止","urgent":true`,
		`"text":"无截止信息","urgent":false`,
		`"text":"not-a-date","urgent":false`, // 脏值原样透出，不抛异常
	} {
		if !strings.Contains(got, must) {
			t.Fatalf("compensation deadline 缺少 %s\n实际输出: %s", must, got)
		}
	}
}

// TestAppJSRenderCompensationErrorVisibility 钉住本 PR 的核心命题：
// **查询失败的账号必须显形**。上游 5xx / token 过期时所有账号都会落到 error 分支，
// 若渲染把它们和「没有补偿」一起过滤掉，卡片整块消失，用户看到「什么都没有」——
// 而真相是「一个都没查成」。这正是 issue #189 要消灭的静默失败。
func TestAppJSRenderCompensationErrorVisibility(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; compensation render test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function compPending');
const end = src.indexOf('function renderCachedCompensation');
if (start < 0 || end < 0 || end < start) throw new Error('render region not found');
const sinks = {};
const mk = id => (sinks[id] = { innerHTML: '', textContent: '', hidden: true });
const ctx = {
  esc: s => String(s == null ? '' : s),
  fmtTok: v => String(v == null ? 0 : v),
  $: id => (sinks[id] || mk(id)),
  Date, Math, Number, String, isFinite, Array, Object,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.render = renderCompensation;', ctx);

const out = {};
// ① 全部查询失败 → 卡片必须可见，且列出失败原因
ctx.render({ accounts: [
  { uid: 'a1', nickname: '甲', error: 'boom-1' },
  { uid: 'b2', nickname: '乙', error: 'boom-2' },
] });
out.allFailedVisible = sinks['compBox'].hidden === false;
out.allFailedNote    = sinks['compNote'].textContent;
out.allFailedShows   = sinks['compList'].innerHTML.includes('boom-1');

// ② 无活动且无失败 → 卡片隐藏（不能变成常驻空卡片）
ctx.render({ accounts: [{ uid: 'a1', nickname: '甲', status: { active: false, claimed: false, credit_num: 0 } }] });
out.quietHidden = sinks['compBox'].hidden === true;

// ③ 混合态：一条待领 + 一条失败 → 可见，且 note 同时提到待领数与失败数
ctx.render({ accounts: [
  { uid: 'a1', nickname: '甲', status: { active: true, claimed: false, credit_num: 500, end_time: '2026-10-22 23:59:59' } },
  { uid: 'b2', nickname: '乙', error: 'boom-2' },
] });
out.mixedVisible = sinks['compBox'].hidden === false;
out.mixedNote    = sinks['compNote'].textContent;
out.mixedHasBtn  = sinks['compList'].innerHTML.includes('data-comp-uid');

// ④ 已领取 → 无「领取」按钮
ctx.render({ accounts: [
  { uid: 'a1', nickname: '甲', status: { active: true, claimed: true, credit_num: 500 } },
] });
out.claimedNoBtn = !sinks['compList'].innerHTML.includes('data-comp-uid');

process.stdout.write(JSON.stringify(out));`
	f, err := os.CreateTemp(t.TempDir(), "comprender-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("compensation render node test failed: %v\n%s", err, out)
	}
	got := strings.TrimSpace(string(out))
	for _, must := range []string{
		`"allFailedVisible":true`, // ① 全失败必须显形
		`"allFailedShows":true`,   // ① 且要带出失败原因
		`"quietHidden":true`,      // ② 真没活动才隐藏
		`"mixedVisible":true`,     // ③ 混合态可见
		`"mixedHasBtn":true`,      // ③ 待领行仍有按钮
		`"claimedNoBtn":true`,     // ④ 已领无按钮
	} {
		if !strings.Contains(got, must) {
			t.Fatalf("render 断言缺少 %s\n实际输出: %s", must, got)
		}
	}
	// note 必须在混合态里同时说出「待领」与「查询失败」两件事。
	if !strings.Contains(got, "查询失败") {
		t.Errorf("note 未提示查询失败: %s", got)
	}
}
