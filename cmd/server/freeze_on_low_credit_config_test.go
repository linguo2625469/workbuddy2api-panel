// freeze_on_low_credit_config_test.go pool.freeze_on_low_credit 配置测试：
// 缺省 true（国内账号保持现状）/ 键缺席仍 true / 显式 false 关闭。
package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFreezeOnLowCreditDefault 缺省 true：Default()+normalize 后低积分冻结开启，
// 保证老部署（配置里无此键）行为与引入前一致。
func TestFreezeOnLowCreditDefault(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !c.Pool.FreezeOnLowCredit {
		t.Error("freeze_on_low_credit 缺省必须为 true（国内账号保持现状）")
	}
}

// TestFreezeOnLowCreditKeyAbsentKeepsTrue 空 JSON 经 ParseConfig（Default 为底再反序列化）
// 不得把缺省 true 抹成 false——这是"缺省 true 靠显式赋值"能成立的关键。
func TestFreezeOnLowCreditKeyAbsentKeepsTrue(t *testing.T) {
	c, err := ParseConfig([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Pool.FreezeOnLowCredit {
		t.Error("键缺席时 freeze_on_low_credit 应保留缺省 true")
	}
}

// TestFreezeOnLowCreditParsedFalseFromFile 显式 false 覆盖默认。
func TestFreezeOnLowCreditParsedFalseFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	if err := os.WriteFile(fp, []byte(`{"pool":{"freeze_on_low_credit":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Pool.FreezeOnLowCredit {
		t.Error("freeze_on_low_credit=false 未生效")
	}
}
