package mobile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// testAuthDoc 生成与插件 OAuth 输出同形的账号文件内容。
func testAuthDoc(t *testing.T, uid, nick, realm, accessToken, refreshToken string, expiresAt int64, enterpriseID string) string {
	t.Helper()
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  accessToken,
			"refreshToken": refreshToken,
			"expiresAt":    expiresAt,
			"domain":       "codebuddy.cn",
			"realm":        realm,
		},
		"account": map[string]any{
			"uid":          uid,
			"nickname":     nick,
			"enterpriseId": enterpriseID,
		},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal auth doc: %v", err)
	}
	return string(raw)
}

func TestImportAndList(t *testing.T) {
	dir := t.TempDir()
	e := NewEngine(dir)

	out, err := e.ImportAccount(testAuthDoc(t, "u1", "一号", "cn", "at-1", "rt-1", 9999999999, ""))
	if err != nil {
		t.Fatalf("import nested: %v", err)
	}
	var acc accountJSON
	if err := json.Unmarshal([]byte(out), &acc); err != nil {
		t.Fatalf("decode account json: %v", err)
	}
	if acc.UID != "u1" || acc.Nickname != "一号" || acc.Realm != "cn" || acc.Enterprise || acc.File != "workbuddy-u1.json" {
		t.Fatalf("unexpected account: %+v", acc)
	}

	flat := `{"accessToken":"at-2","refreshToken":"rt-2","expiresAt":9999999999,"uid":"u2","nickname":"二号"}`
	if _, err := e.ImportAccount(flat); err != nil {
		t.Fatalf("import flat: %v", err)
	}

	raw, err := e.ListAccounts()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var list []accountJSON
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list) != 2 || list[0].UID != "u1" || list[1].UID != "u2" {
		t.Fatalf("unexpected list: %+v", list)
	}
}

func TestImportRejectsInvalid(t *testing.T) {
	e := NewEngine(t.TempDir())
	cases := []string{
		`not json`,
		`{"auth":{"accessToken":""},"account":{"uid":"u1"}}`,
		`{"accessToken":"at","refreshToken":"rt"}`,
	}
	for _, c := range cases {
		if _, err := e.ImportAccount(c); err == nil {
			t.Fatalf("want error for %s", c)
		}
	}
}

func TestImportOverwritesSameUID(t *testing.T) {
	dir := t.TempDir()
	e := NewEngine(dir)
	if _, err := e.ImportAccount(testAuthDoc(t, "u1", "旧", "cn", "at-old", "rt-old", 9999999999, "")); err != nil {
		t.Fatalf("first import: %v", err)
	}
	if _, err := e.ImportAccount(testAuthDoc(t, "u1", "新", "cn", "at-new", "rt-new", 9999999999, "")); err != nil {
		t.Fatalf("second import: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "workbuddy-u1.json"))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	var doc struct {
		Auth struct {
			AccessToken string `json:"accessToken"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode file: %v", err)
	}
	if doc.Auth.AccessToken != "at-new" {
		t.Fatalf("accessToken=%q want at-new", doc.Auth.AccessToken)
	}
}
