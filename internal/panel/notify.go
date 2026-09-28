// notify.go 通知渠道测试发送：面板「通知」页对单个渠道发测试消息。
//
// 两种模式：
//   - 按名称：body {"name": "钉钉群"} —— 测已保存（热生效中）的渠道；
//   - 临时配置：body {"type": "...", "config": {...}} —— 表单未保存先试发。
// 渠道的增删改走通用 /panel/api/config（notifications 段，保存即热生效），
// 此接口只承担「发送测试」这一件事。
package panel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/notify"
)

type notifyTestReq struct {
	Name   string         `json:"name"`
	Type   string         `json:"type"`
	Config map[string]any `json:"config"`
}

// notifyTest 发送测试消息（同步等待渠道响应，最长 20s，前端据此显示成败）。
func (p *Panel) notifyTest(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Notifier == nil {
		writeErr(w, http.StatusNotImplemented, "notifier not available")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var req notifyTestReq
	if err := json.Unmarshal(raw, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "parse body: "+err.Error())
		return
	}

	var ch notify.Channel
	if req.Name != "" {
		found := false
		for _, c := range p.cfg.Notifier.Channels() {
			if c.Name == req.Name {
				ch = c
				found = true
				break
			}
		}
		if !found {
			writeErr(w, http.StatusNotFound, "渠道不存在: "+req.Name)
			return
		}
	} else {
		if req.Type == "" {
			writeErr(w, http.StatusBadRequest, "缺少 name 或 type")
			return
		}
		ch = notify.Channel{Name: "（未保存）", Type: req.Type, Config: req.Config, Enabled: true}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	if err := p.cfg.Notifier.Test(ctx, ch); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
