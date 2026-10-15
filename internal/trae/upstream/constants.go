// constants.go SOLO 上游技术常量（SPEC §1，来自实测，禁止改动）。
package upstream

const (
	AgentHost      = "https://trae-api-cn.mchost.guru"
	UgHost         = "https://api.trae.cn"
	OAuthHost      = "https://api.trae.com.cn"
	ConsoleHost    = "https://www.trae.cn"
	ClientID       = "en1oxy7wnw8j9n" // SOLO stable
	AppID          = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	IdeVersion     = "0.1.52"
	IdeVersionCode = "20260811"
	DeviceBrand    = "83DG"
	OSVersion      = "Windows 11 Pro"
	Function       = "solo_work_lite"
	// FunctionAgent Agent 任务作用域（IDE「Agent」选模型列表），模型目录比
	// solo_work_lite 多出 glm-5.3-flashx / glm-5.3-flash / qwen3.8-flash 等；
	// 实测 chat 对该作用域模型同样放行（2026-10 探针验证）。
	FunctionAgent = "solo_agent"

	// 端点
	EpChat          = "/api/agent/v3/llm_utils_chat"
	EpModels        = "/api/ide/v1/get_detail_param"
	EpExchange      = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	EpUserInfo      = "/cloudide/api/v3/trae/GetUserInfo"
	EpCheckinStatus = "/trae/api/v2/ug/checkin_credits/status"
	EpCheckinClaim  = "/trae/api/v2/ug/checkin_credits/claim"
	EpEntUsage      = "/trae/api/v2/pay/ide_user_ent_usage"
)
