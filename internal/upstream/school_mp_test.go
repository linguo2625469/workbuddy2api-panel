package upstream

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestMPEventBase(t *testing.T) {
	a := &auth.Auth{UID: "u-1", Nickname: "测试"}
	base := mpEventBase(a)
	for _, k := range []string{"ideType", "extName", "ideName", "platform", "userId"} {
		if _, ok := base[k]; !ok {
			t.Errorf("missing common field %s", k)
		}
	}
	if base["ideType"] != "WorkBuddy_MP" || base["extName"] != "workbuddy-mp" {
		t.Errorf("fingerprint ideType=%v extName=%v", base["ideType"], base["extName"])
	}
}

func TestSchoolChatTimesEvents(t *testing.T) {
	ev := SchoolChatTimesEvents("conv-1")
	if ev["eventCode"] != "chat_request_send" {
		t.Errorf("eventCode=%v", ev["eventCode"])
	}
	if ev["conversationId"] != "conv-1" || ev["codebuddy.session_id"] != "conv-1" {
		t.Errorf("conversation join fields missing: %v", ev)
	}
	b, _ := json.Marshal(ev)
	if !strings.Contains(string(b), "agentName") {
		t.Error("agentName missing")
	}
}
