package server

// 旧模型自动淘汰：清单跟着上游走，请求路径跟着清单走。
//
// 两条纪律：
//  1. 只有"最近一次成功拉取"的清单才有资格拒绝请求（fresh）——
//     拉取失败时 entries 会保留旧数据，那份可能已经被上游下架；
//  2. 拒绝必须发生在选号/建会话之前，否则每个死名字仍要花 4~5 秒。

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

func TestModelLookupFreshness(t *testing.T) {
	c := NewModelCache(nil, nil) // Lookup 不碰 up/pool

	// 从没成功拉过 ⇒ 不可信，调用方必须放行。
	if _, fresh := c.Lookup("qwen3.7-plus"); fresh {
		t.Fatal("首拉成功之前不得据此拒绝请求")
	}

	// 一次成功拉取之后才可信。
	live := model("qwen3.7-plus", "Qwen3.7-Plus", true, true, 1000)
	dead := model("qwen3.5-plus", "Qwen3.5-Plus", true, true, 1000)
	dead.Info.IsActive = false
	c.mu.Lock()
	c.entries = []upstream.ModelEntry{live, dead}
	c.lastOK = time.Now()
	c.mu.Unlock()

	if known, fresh := c.Lookup("qwen3.7-plus"); !known || !fresh {
		t.Fatalf("在清单里的模型应 known+fresh，得到 known=%v fresh=%v", known, fresh)
	}
	if known, fresh := c.Lookup("qwen3.5-plus"); known || !fresh {
		t.Fatalf("上游标记停用的模型应 known=false（fresh=true），得到 known=%v fresh=%v", known, fresh)
	}

	// 清单太旧（超过 2×ttl）⇒ 退回不可信，避免拿过期快照误杀新模型。
	c.mu.Lock()
	c.lastOK = time.Now().Add(-3 * c.ttl)
	c.mu.Unlock()
	if _, fresh := c.Lookup("qwen3.7-plus"); fresh {
		t.Fatal("清单过期太久不应再作为拒绝依据")
	}
}

// 请求已下架的模型：必须立刻 400，且一次 completions 都不打。
func TestDeprecatedModelRejectedBeforeUpstream(t *testing.T) {
	up := newFakeUpstream(t, 0)
	up.dynamicChats = true
	s := newTestServerWithServer(t, up, 1)
	h := s.Handler()

	// 装一份"可信清单"，其中没有 qwen3.5-plus。
	s.models.mu.Lock()
	s.models.entries = []upstream.ModelEntry{model("qwen3.7-plus", "Qwen3.7-Plus", true, true, 1000)}
	s.models.lastOK = time.Now()
	s.models.mu.Unlock()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, agentReq(`{"model":"qwen3.5-plus","stream":false,
		"messages":[{"role":"user","content":"你好"}]}`))

	if rec.Code != 400 {
		t.Fatalf("已下架模型应回 400（调用方的错），得到 %d", rec.Code)
	}
	if up.gotSubmit != nil {
		t.Fatal("不该打上游 completions：应该在网关侧就拒绝")
	}
	if up.newChats != 0 {
		t.Fatalf("不该建会话，实际建了 %d 次", up.newChats)
	}
}
