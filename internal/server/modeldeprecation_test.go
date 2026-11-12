package server

// 旧模型自动淘汰：清单跟着上游走，请求路径跟着清单走。
//
// 两条纪律：
//  1. 只有"最近一次成功拉取"的清单才有资格拒绝请求（fresh）——
//     拉取失败时 entries 会保留旧数据，那份可能已经被上游下架；
//  2. 拒绝必须发生在选号/建会话之前，否则每个死名字仍要花 4~5 秒。

import (
	"encoding/json"
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

// 别名：上游按新名跑，响应仍回显客户端原名。
//
// 这样上游下架模型时，网关一行配置就能救活老客户端，而客户端完全不感知
// （响应里的 model 还是它发的那个名字）。
func TestModelAliasRoutesButEchoesOriginal(t *testing.T) {
	up := newFakeUpstream(t, 0)
	up.dynamicChats = true
	s := newTestServerWithServer(t, up, 1)

	cfg := s.conf()
	cfg.ModelAliases = map[string]string{"qwen3.5-plus": "qwen3.7-plus"}
	s.cfg.Store(cfg)
	// 可信清单里有目标模型，没有别名（否则前面那道淘汰会把请求拦掉）。
	s.models.mu.Lock()
	s.models.entries = []upstream.ModelEntry{model("qwen3.7-plus", "Qwen3.7-Plus", true, true, 1000)}
	s.models.lastOK = time.Now()
	s.models.mu.Unlock()

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, agentReq(`{"model":"qwen3.5-plus","stream":false,
		"messages":[{"role":"user","content":"你好"}]}`))

	if rec.Code != 200 {
		t.Fatalf("别名应让请求成功，得到 %d：%s", rec.Code, rec.Body.String())
	}
	if up.gotSubmit == nil || up.gotSubmit["model"] != "qwen3.7-plus" {
		t.Fatalf("上游应收到目标模型，实际 %v", up.gotSubmit["model"])
	}
	var resp struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Model != "qwen3.5-plus" {
		t.Fatalf("响应必须回显客户端原名，得到 %q", resp.Model)
	}
	// 流水里要能看到它被路由到了哪 —— 否则"老名字其实跑在新模型上"不可查。
	rows := s.log.Recent(1)
	if len(rows) != 1 || rows[0].Alias != "qwen3.5-plus→qwen3.7-plus" {
		t.Fatalf("流水应记录别名，得到 %+v", rows)
	}
}
