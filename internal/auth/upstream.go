package auth

// 上游入口常量（OAuth 设备授权流程专用）。
//
// 为什么放这里：cmd/login 与 internal/panel 的登录流程各存了一份逐字相同的常量组，
// 两份 base/Origin/UA 正是「改了一处忘了另一处」的温床。auth 被两者共同依赖，
// 是唯一不会引入反向依赖的归属。
//
// 注意 CLI/2.63.2 是登录端点的固定取值，**不要**与 internal/upstream 的
// defaultCliVersion(2.137.1) 或 desktopUA(2.137.1) 统一——后者是桌面上行 UA，
// 前者是设备授权端点认的版本，两者取值不同是有意的。
const (
	// UpstreamBaseCN 国内版上游 base（Origin 为 codebuddy.cn）。
	UpstreamBaseCN = "https://copilot.tencent.com"
	// UpstreamBaseGlobal 国际版上游 base（base 与 Origin/Referer 同域）。
	UpstreamBaseGlobal = "https://www.workbuddy.ai"
	// ClientUA 设备授权端点请求的 User-Agent。
	ClientUA = "CLI/2.63.2 CodeBuddy/2.63.2"
	// OriginRefererCN 国内版 Origin/Referer 基础域。
	OriginRefererCN = "https://www.codebuddy.cn"
	// OriginRefererGlobal 国际版 Origin/Referer 基础域。
	OriginRefererGlobal = "https://www.workbuddy.ai"
)
