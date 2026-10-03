package responses

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"time"
)

// idSeq 进程内自增序号，保证同一毫秒内生成的 ID 不重复。
var idSeq atomic.Uint64

// newID 生成 Responses 风格的短 ID（resp_ / msg_ / fc_ / ctc_ / rs_ / ws_ / call_）。
func newID(prefix string) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 熵源不可用时退回时间+序号，绝不 panic（ID 只用于客户端关联）。
		return fmt.Sprintf("%s%d%04x", prefix, time.Now().UnixNano(), idSeq.Add(1)&0xffff)
	}
	return prefix + hex.EncodeToString(b[:])
}

// nowUnix 返回当前 Unix 秒（便于测试替换）。
var nowUnix = func() int64 { return time.Now().Unix() }
