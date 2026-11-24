// global_register.go 国际版（global realm）新账号注册激活与地区完善。
//
// 背景（ANALYSIS-workbuddy-client-reverse.md）：新 global 账号需先完成注册地区
// （/login/register/user/complete 补地区）再调 register 接口激活 Trial，chat 才不报
// 14017 trial not activated。链路（逆向自 web 注册完善页 RegisterRegion-*.js）：
//
//	POST /billing/area/get-country-code {filterForbidden:1} → 可取国家列表
//	POST /billing/area/get-user-area-info {action:getUserAreaInfo} → 检测当前地区
//	POST /console/login/account {attributes:{countryCode,countryFullName,countryName}} → 提交地区（幂等）
//	GET  /auth/realms/copilot/overseas/user/register?userId=<uid> → 注册激活（code:200 成功；code:500 "region required" 需补地区）
//	POST /billing/ide/trial → 一次性加油包（幂等码 14051，见 trial.go）
//
// 响应体注意：get-country-code / get-user-area-info 的 data 是 JSON 字符串
// （双层信封），需二次解析。
package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// globalWebUA 国际版 web 端 UA（注册完善页走 web 指纹，非桌面端 CLI 指纹）。
const globalWebUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

// GlobalCountry 可选注册地区（对应 get-country-code 的 list 元素）。
type GlobalCountry struct {
	EnName string `json:"EnName"` // 英文全名（countryFullName）
	Name   string `json:"Name"`   // 显示名
	IOS2   string `json:"IOS2"`   // 二字码（countryName）
	IOS3   string `json:"IOS3"`
	Code   string `json:"Code"` // 数字码（countryCode）
}

// globalRegisterBase 注册激活端点的 base（注册链路在 www.workbuddy.ai，与 globalBillingBase 同域）。
// 独立成方法便于测试替换。
func (c *Client) globalRegisterBase() string {
	if c.BillingBaseGlobal != "" {
		return c.BillingBaseGlobal
	}
	return defaultGlobalBase
}

// globalRegisterReq 注册链路通用请求构造：web 指纹 UA + Origin/Referer 同域 + Bearer。
func (c *Client) globalRegisterReq(method, url, token string, body any) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return nil, err
	}
	base := c.globalRegisterBase()
	req.Header.Set("User-Agent", globalWebUA)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", base)
	req.Header.Set("Referer", base+"/")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req, nil
}

// globalRegisterJSON 发注册链路请求并解外层信封（code/msg）。
func (c *Client) globalRegisterJSON(req *http.Request) (code int, msg string, raw json.RawMessage, err error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, "", nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return 0, "", nil, fmt.Errorf("global register parse: %w", err)
	}
	return env.Code, env.Msg, env.Data, nil
}

// GlobalFetchCountries 拉取可选注册地区列表（global 账号登录后调用）。
// intlOnly=true 时按国际版 web 白名单过滤（HK/MO/SG/TH/PH/MY/ID，对齐 web 展示集）。
func (c *Client) GlobalFetchCountries(a *auth.Auth, intlOnly bool) ([]GlobalCountry, error) {
	if a == nil || a.Realm() != "global" {
		return nil, fmt.Errorf("fetch countries: only global accounts")
	}
	req, err := c.globalRegisterReq(http.MethodPost, c.globalRegisterBase()+"/billing/area/get-country-code", a.AccessTokenValue(), map[string]any{"filterForbidden": 1})
	if err != nil {
		return nil, err
	}
	c.injectResinAccount(req, a)
	code, msg, raw, err := c.globalRegisterJSON(req)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("get-country-code: %s (code=%d)", msg, code)
	}
	// data 是 JSON 字符串（双层信封）或对象，需二次解析。
	var inner struct {
		Data struct {
			List []GlobalCountry `json:"list"`
		} `json:"data"`
	}
	if s := strings.TrimSpace(string(raw)); strings.HasPrefix(s, "\"") {
		var s2 string
		if err := json.Unmarshal(raw, &s2); err != nil {
			return nil, fmt.Errorf("country list unwrap: %w", err)
		}
		raw = json.RawMessage(s2)
	}
	if err := json.Unmarshal(raw, &inner); err != nil {
		return nil, fmt.Errorf("country list parse: %w", err)
	}
	list := inner.Data.List
	if !intlOnly {
		return list, nil
	}
	// 国际版 web 白名单过滤（HK, MO, SG, TH, PH, MY, ID，顺序对齐 web 展示）。
	whitelist := []string{"HK", "MO", "SG", "TH", "PH", "MY", "ID"}
	byCode := make(map[string]GlobalCountry, len(list))
	for _, ctry := range list {
		byCode[ctry.IOS2] = ctry
	}
	out := make([]GlobalCountry, 0, len(whitelist))
	for _, code := range whitelist {
		if ctry, ok := byCode[code]; ok {
			out = append(out, ctry)
		}
	}
	return out, nil
}

// GlobalRegisterStatus 报告 global 账号注册激活状态：是否需要补地区、是否已激活。
func (c *Client) GlobalRegisterStatus(a *auth.Auth) (activated bool, needsRegion bool, msg string, err error) {
	if a == nil || a.Realm() != "global" {
		return false, false, "", fmt.Errorf("register status: only global accounts")
	}
	req, err := c.globalRegisterReq(http.MethodGet,
		c.globalRegisterBase()+"/auth/realms/copilot/overseas/user/register?userId="+a.UID,
		a.AccessTokenValue(), nil)
	if err != nil {
		return false, false, "", err
	}
	req.Header.Set("X-User-Id", a.UID)
	c.injectResinAccount(req, a)
	code, m, _, err := c.globalRegisterJSON(req)
	if err != nil {
		return false, false, "", err
	}
	switch {
	case code == 200:
		return true, false, "register success", nil
	case code == 500 || strings.Contains(strings.ToLower(m), "region required"):
		return false, true, m, nil
	default:
		return false, false, m, nil
	}
}

// GlobalSubmitRegion 提交注册地区（幂等）。country 来自 GlobalFetchCountries。
func (c *Client) GlobalSubmitRegion(a *auth.Auth, country GlobalCountry) error {
	if a == nil || a.Realm() != "global" {
		return fmt.Errorf("submit region: only global accounts")
	}
	attrs := map[string]any{
		"countryCode":     []string{country.Code},
		"countryFullName": []string{country.EnName},
		"countryName":     []string{country.IOS2},
	}
	req, err := c.globalRegisterReq(http.MethodPost, c.globalRegisterBase()+"/console/login/account",
		a.AccessTokenValue(), map[string]any{"attributes": attrs})
	if err != nil {
		return err
	}
	c.injectResinAccount(req, a)
	code, msg, _, err := c.globalRegisterJSON(req)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("submit region: %s (code=%d)", msg, code)
	}
	return nil
}

// GlobalCompleteRegistration 一键注册激活：先查状态，需补地区则按默认地区（白名单首个，
// 通常 HK）提交后重新激活。activated 表示调用后账号已激活可用。幂等（已激活直接返回）。
// 失败不阻断调用方（panel login 已落盘），仅返回错误供日志。
func (c *Client) GlobalCompleteRegistration(a *auth.Auth) (activated bool, err error) {
	activated, needsRegion, msg, err := c.GlobalRegisterStatus(a)
	if err != nil {
		return false, err
	}
	if activated {
		return true, nil
	}
	if !needsRegion {
		return false, fmt.Errorf("register not activated: %s", msg)
	}
	// 需补地区：拉白名单，取首个（HK）提交。
	countries, err := c.GlobalFetchCountries(a, true)
	if err != nil {
		return false, fmt.Errorf("fetch countries: %w", err)
	}
	if len(countries) == 0 {
		return false, fmt.Errorf("no countries available")
	}
	if err := c.GlobalSubmitRegion(a, countries[0]); err != nil {
		return false, fmt.Errorf("submit region: %w", err)
	}
	// 重新激活验证。
	activated, needsRegion, msg, err = c.GlobalRegisterStatus(a)
	if err != nil {
		return false, err
	}
	if !activated {
		return false, fmt.Errorf("register still not activated after region submit: %s", msg)
	}
	return true, nil
}
