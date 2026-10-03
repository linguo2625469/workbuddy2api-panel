package server

import (
	"errors"
	"io"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/responses"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// streamAdapter 抽象「上游 Chat 流 → 客户端协议」的差异。
//
// 存在的意义：/v1/chat/completions 与 /v1/responses 共享同一条选号、轮转、
// 提示词改写、错误处置与用量记账管线——两套协议只在「回程怎么写」上不同。
// 把差异收进这个接口，避免复制 500 行管线代码（复制出来的第二份迟早漂移）。
type streamAdapter interface {
	// WriteStream 把上游 Chat SSE 流转换成客户端协议写入 w。
	// hintFn 仅供 Chat 路径在 error 帧上附加 gateway_hint。
	WriteStream(w http.ResponseWriter, src io.Reader, hintFn func(string) string) error
	// WriteNonStream 把聚合后的 Chat 响应转换成客户端协议写入 w。
	WriteNonStream(w http.ResponseWriter, resp map[string]any)
	// EmptyStream 判定「上游 200 但零有效帧」——两条路径的哨兵不同。
	EmptyStream(err error) bool
}

// chatAdapter 默认路径：Chat Completions 原样透传（含既有 SSE 归一化与 hint）。
type chatAdapter struct{}

func (chatAdapter) WriteStream(w http.ResponseWriter, src io.Reader, hintFn func(string) string) error {
	return upstream.StreamHint(w, src, hintFn)
}

func (chatAdapter) WriteNonStream(w http.ResponseWriter, resp map[string]any) {
	writeJSON(w, http.StatusOK, resp)
}

func (chatAdapter) EmptyStream(err error) bool { return upstream.IsEmptyStreamError(err) }

// responsesAdapter 把上游 Chat 输出翻译回 Responses 形态。
type responsesAdapter struct {
	meta  *responses.Meta
	model string
}

func (a responsesAdapter) WriteStream(w http.ResponseWriter, src io.Reader, _ func(string) string) error {
	return responses.Stream(w, src, a.meta, a.model)
}

func (a responsesAdapter) WriteNonStream(w http.ResponseWriter, resp map[string]any) {
	writeJSON(w, http.StatusOK, responses.FromChat(resp, a.meta, a.model))
}

func (a responsesAdapter) EmptyStream(err error) bool { return errors.Is(err, responses.ErrEmptyStream) }
