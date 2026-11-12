package server

// 视频前门的端到端测试。
//
// 视频是**花钱**的动作：一次 720p/5s 就是 30 积分，而充值/余额/档位这些分支
// 恰恰是「看起来通、实际误导」的重灾区。所以这里全部对应假上游，零消耗。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/yuanjie-ai/qwen2api/internal/config"
	"github.com/yuanjie-ai/qwen2api/internal/pool"
	"github.com/yuanjie-ai/qwen2api/internal/reqlog"
	"github.com/yuanjie-ai/qwen2api/internal/upstream"
)

// videoUpstream 假上游：建会话 + 提交任务 + 查询状态 + 积分余额。
type videoUpstream struct {
	srv *httptest.Server
	mu  sync.Mutex
	// balance 返回给网关的积分余额。
	balance int
	// creditsFail 为 true 时积分接口 500（模拟读不到余额）。
	creditsFail bool
	submits     int
	// taskStatus 查询端点返回的状态原文；"" 时返回 404（模拟还没开始）。
	taskStatus string
	// productURL 查询端点返回的产物。
	productURL string
	// lastSubmit 上游收到的提交体（断言参数下没下发用）。
	lastSubmit map[string]any
}

func newVideoUpstream(t *testing.T, balance int) *videoUpstream {
	t.Helper()
	f := &videoUpstream{balance: balance, taskStatus: "QUEUED"}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/api/entitlement/credits/credits/history"):
			f.mu.Lock()
			fail, bal := f.creditsFail, f.balance
			f.mu.Unlock()
			if fail {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"httpCode":200,"success":true,"data":{"totalCredits":` +
				itoa(bal) + `,"subscriptionCredits":0,"bonusCredits":0}}`))
		case strings.Contains(r.URL.Path, "/api/v2/chats/new"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"data":{"id":"chat-1"}}`))
		case strings.Contains(r.URL.Path, "/api/v2/chat/completions"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.submits++
			f.lastSubmit = body
			f.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"data":{"messages":[
				{"extra":{"wanx":{"task_id":"tsk_test123"}}}]}}`))
		case strings.Contains(r.URL.Path, "/api/v2/task/status/"):
			f.mu.Lock()
			st, product := f.taskStatus, f.productURL
			f.mu.Unlock()
			if st == "" {
				http.NotFound(w, r)
				return
			}
			data := `{"task_status":"` + st + `"`
			if product != "" {
				data += `,"content":"` + product + `"`
			}
			data += `}`
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"data":` + data + `}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *videoUpstream) submitCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.submits
}

func (f *videoUpstream) setStatus(status, product string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.taskStatus, f.productURL = status, product
}

func newVideoServer(t *testing.T, up *videoUpstream, mutate func(*config.Config)) http.Handler {
	t.Helper()
	cfg := config.Default()
	cfg.Upstream.BaseURL = up.srv.URL
	cfg.Upstream.AuthBaseURL = up.srv.URL
	cfg.Upstream.EntitlementBaseURL = up.srv.URL
	// 空 StateFile ⇒ 不落盘、不起后台巡检（测试要确定性）。
	cfg.StateFile = ""
	cfg.Accounts = []config.Account{{Name: "a0", Token: "tok-0"}}
	if mutate != nil {
		mutate(&cfg)
	}
	p := pool.New(cfg.Pool, cfg.Accounts, "")
	cli := upstream.New(cfg.Upstream)
	return New(cfg, p, cli, reqlog.New(10), "test").Handler()
}

// videoBody 构造一份最小合法 canonical 请求。
func videoBody(extra string) string {
	body := `{"model":"happyhorse11","content":[{"type":"text","text":"a cat"}],"resolution":"720p","duration":5`
	if extra != "" {
		body += "," + extra
	}
	return body + "}"
}

func postVideo(h http.Handler, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, videoTaskPath, strings.NewReader(body)))
	return rec
}

// TestVideoCreateResponseOnlyID 创建回包**只有 id**。
//
// 这条钉的是协议里最容易被「优化」坏的一条：受理 ≠ 成功。
// 往里塞 status/model 等于承诺一个客户端不该依赖的形状，
// 而客户端一旦依赖，上游换词表时就是一次静默的行为变更。
func TestVideoCreateResponseOnlyID(t *testing.T) {
	up := newVideoUpstream(t, 100)
	h := newVideoServer(t, up, nil)

	rec := postVideo(h, videoBody(""))
	if rec.Code != http.StatusOK {
		t.Fatalf("应成功，得到 %d: %s", rec.Code, rec.Body.String())
	}
	var obj map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &obj); err != nil {
		t.Fatal(err)
	}
	if len(obj) != 1 {
		t.Fatalf("创建回包只该有 id，实际 %v", obj)
	}
	id, _ := obj["id"].(string)
	if !strings.HasPrefix(id, "cgt-") {
		t.Fatalf("id 应为方舟格式 cgt-<时间戳>-<随机>: %v", obj)
	}
	// 方舟原生形态：cgt-20261105010835-xxxxx（时间戳 14 位 + 8 位十六进制）。
	if len(strings.Split(id, "-")) != 3 || len(strings.Split(id, "-")[1]) != 14 {
		t.Fatalf("id 形状不对: %q", id)
	}
}

func TestVideoRejectedWhenBalanceTooLow(t *testing.T) {
	// 余额 10 分，请求 720p 5s（30 分）。必须本地就拦掉 ——
	// 放过去只会拿回「Credits 不足」，白跑一次上游，还可能把账号记成一次失败。
	up := newVideoUpstream(t, 10)
	h := newVideoServer(t, up, nil)

	rec := postVideo(h, videoBody(""))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("余额不足应 429，得到 %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "insufficient_quota") {
		t.Fatalf("错误类型应为 insufficient_quota: %s", body)
	}
	if !strings.Contains(body, "a0=10") {
		t.Fatalf("文案应说明具体余额（否则用户不知道该充多少）: %s", body)
	}
	if n := up.submitCount(); n != 0 {
		t.Fatalf("余额不足时不该提交上游，实际提交 %d 次", n)
	}
}

func TestVideoSucceedsAndDebitsBalance(t *testing.T) {
	up := newVideoUpstream(t, 100)
	h := newVideoServer(t, up, nil)

	rec := postVideo(h, videoBody(""))
	if rec.Code != http.StatusOK {
		t.Fatalf("应成功，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if n := up.submitCount(); n != 1 {
		t.Fatalf("应提交 1 次，实际 %d", n)
	}

	// /stats 里应看到余额已被乐观扣减到 70
	srec := httptest.NewRecorder()
	h.ServeHTTP(srec, httptest.NewRequest(http.MethodGet, "/stats", nil))
	if !strings.Contains(srec.Body.String(), `"credit_balance":70`) {
		t.Fatalf("乐观扣减未生效: %s", srec.Body.String()[:min(400, len(srec.Body.String()))])
	}
}

func TestVideoAllowedWhenBalanceUnreadable(t *testing.T) {
	// 读不到余额时必须**放行**，靠上游 insufficient_quota 兜底。
	// 反过来（默认拦死）等于一次接口抖动就停掉整个视频能力。
	up := newVideoUpstream(t, 0)
	up.creditsFail = true
	h := newVideoServer(t, up, nil)

	rec := postVideo(h, videoBody(""))
	if rec.Code != http.StatusOK {
		t.Fatalf("余额读不到时应放行，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if n := up.submitCount(); n != 1 {
		t.Fatalf("应提交 1 次，实际 %d", n)
	}
}

// TestVideoRejectsUnknownTier 7 秒不是上游支持的档位（只有 5/10/15）。
// 这种请求不该放过去烧积分。
func TestVideoRejectsUnknownTier(t *testing.T) {
	up := newVideoUpstream(t, 100)
	h := newVideoServer(t, up, nil)

	rec := postVideo(h,
		`{"model":"happyhorse11","content":[{"type":"text","text":"猫"}],"resolution":"720p","duration":7}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法档位应 400，得到 %d: %s", rec.Code, rec.Body.String())
	}
	var e struct {
		Error struct {
			Code  string `json:"code"`
			Param string `json:"param"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e.Error.Code != "InvalidParameter.UnsupportedParameter" || e.Error.Param != "duration" {
		t.Fatalf("应报 InvalidParameter.UnsupportedParameter/duration，实际 %+v", e.Error)
	}
	if n := up.submitCount(); n != 0 {
		t.Fatalf("非法档位不该提交上游，实际 %d", n)
	}
}

func TestVideoTierPricingMatchesModelVersion(t *testing.T) {
	// 价格表跟着 root_model 走：happyhorse11（默认）720P 5s=30、1080P 15s=120；
	// 1.0 是 40/210。这条钉「别拿错表」这个坑。
	cfg := config.Default()
	cases := []struct {
		res  string
		dur  int
		want int
	}{
		{"720P", 5, 30}, {"720P", 10, 60}, {"720P", 15, 90},
		{"1080P", 5, 40}, {"1080P", 10, 80}, {"1080P", 15, 120},
	}
	for _, c := range cases {
		got, ok := cfg.Video.VideoCredits(c.res, c.dur)
		if !ok || got != c.want {
			t.Fatalf("%s/%ds 应为 %d 分，得到 %d（ok=%v）", c.res, c.dur, c.want, got, ok)
		}
	}
	if cfg.Video.RootModel != "happyhorse11" {
		t.Fatalf("默认 root_model 变了（%s）⇒ 价格表要跟着换", cfg.Video.RootModel)
	}
}

// TestVideoBalanceShortReleasesInflight 余额不足跳过账号时**必须 Release**。
//
// 这条钉的是一个静默 bug：Pick 已把 inflight +1，若 continue 前不释放，
// 这个号会被永久算作「在途」，之后再也选不中 —— 池子越大越难发现
// （只是可用号变少，日志里没有任何报错）。
func TestVideoBalanceShortReleasesInflight(t *testing.T) {
	up := newVideoUpstream(t, 0)
	h := newVideoServer(t, up, nil)

	rec := postVideo(h, videoBody(""))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("余额 0 时视频应 429，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if up.submitCount() != 0 {
		t.Fatalf("余额不足不该提交上游，实际提交 %d 次", up.submitCount())
	}

	srec := httptest.NewRecorder()
	h.ServeHTTP(srec, httptest.NewRequest(http.MethodGet, "/stats", nil))
	var st struct {
		Pool []struct {
			Inflight int `json:"inflight"`
		} `json:"pool"`
	}
	if err := json.Unmarshal(srec.Body.Bytes(), &st); err != nil {
		t.Fatalf("解析 /stats 失败: %v", err)
	}
	for _, a := range st.Pool {
		if a.Inflight != 0 {
			t.Fatalf("余额不足跳过后 inflight 应归零，实际 %d", a.Inflight)
		}
	}
}

// TestVideoValidationOrder 校验顺序固定：content → model → 场景 → 取值域。
//
// 顺序错了，错误信息就指向错的字段：content 为空时去报 model 缺失，
// 调用方会去补 model，然后在下一个错误里再绕一圈。
func TestVideoValidationOrder(t *testing.T) {
	up := newVideoUpstream(t, 100)
	h := newVideoServer(t, up, nil)

	// content 与 model 都缺 ⇒ 必须先报 content。
	rec := postVideo(h, `{"resolution":"720p","duration":5}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("应 400，得到 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"param":"content"`) {
		t.Fatalf("应优先报 content: %s", rec.Body.String())
	}

	// content 有了、model 缺 ⇒ 报 model。
	rec = postVideo(h, `{"content":[{"type":"text","text":"猫"}],"resolution":"720p","duration":5}`)
	if !strings.Contains(rec.Body.String(), `"param":"model"`) {
		t.Fatalf("该报 model 了: %s", rec.Body.String())
	}

	// 场景互斥早于取值域：首帧 + 参考图混用，即使 duration 也非法，必须先报场景。
	rec = postVideo(h, `{"model":"m","resolution":"720p","duration":9,"content":[
		{"type":"text","text":"猫"},
		{"type":"image_url","image_url":{"url":"https://x/a.png"},"role":"first_frame"},
		{"type":"image_url","image_url":{"url":"https://x/b.png"},"role":"reference_image"}]}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"param":"content"`) {
		t.Fatalf("场景混用应先于取值域被拒: %d %s", rec.Code, rec.Body.String())
	}
}

// TestVideoLegacyPathRetired 旧路径显式 410。
//
// 不做静默兼容：旧形状（prompt 平铺 + url 字段 + completed 词表）与 canonical
// 是两套契约，同时存在必然「两边字段对不上」。410 比 404 诚实 ——
// 404 会让调用方以为打错地址，一直重试。
func TestVideoLegacyPathRetired(t *testing.T) {
	up := newVideoUpstream(t, 100)
	h := newVideoServer(t, up, nil)

	// 三代旧路径都要 410：更早的 generations、上一版的 canonical、以及它的查询/删除。
	for _, probe := range []struct{ method, path string }{
		{http.MethodPost, "/v1/videos/generations"},
		{http.MethodPost, "/v1/videos"},
		{http.MethodGet, "/v1/videos/cgt-1"},
		{http.MethodDelete, "/v1/videos/cgt-1"},
		{http.MethodGet, "/v1/videos/cgt-1/content"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(probe.method, probe.path, strings.NewReader(`{"prompt":"猫"}`)))
		if rec.Code != http.StatusGone {
			t.Fatalf("%s %s 应 410，得到 %d: %s", probe.method, probe.path, rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "EndpointRetired") {
			t.Fatalf("%s %s 应带 EndpointRetired: %s", probe.method, probe.path, rec.Body.String())
		}
	}
	if up.submitCount() != 0 {
		t.Fatal("旧路径不该提交上游")
	}
}

// TestVideoGetTranslatesTask 查询把上游状态归一成 canonical 六态，
// 且产物只在 succeeded 时出现。
func TestVideoGetTranslatesTask(t *testing.T) {
	up := newVideoUpstream(t, 100)
	h := newVideoServer(t, up, nil)

	rec := postVideo(h, videoBody(""))
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.ID == "" {
		t.Fatalf("创建失败: %s", rec.Body.String())
	}

	// 还在排队：不该有产物，也不该编一个。
	up.setStatus("RUNNING", "")
	got := getVideo(t, h, created.ID)
	if got["status"] != "running" {
		t.Fatalf("RUNNING 应归一成 running，得到 %v", got["status"])
	}
	if content, _ := got["content"].(map[string]any); content["video_url"] != nil {
		t.Fatalf("非终态不该有 video_url: %v", content)
	}

	// 成功：content.video_url 出现。
	up.setStatus("SUCCESS", "https://cdn.example/v.mp4")
	got = getVideo(t, h, created.ID)
	if got["status"] != "succeeded" {
		t.Fatalf("SUCCESS 应归一成 succeeded，得到 %v", got["status"])
	}
	content, _ := got["content"].(map[string]any)
	if content["video_url"] != "https://cdn.example/v.mp4" {
		t.Fatalf("产物没出来: %v", content)
	}
	if got["error"] != nil {
		t.Fatalf("成功时 error 必须是 null: %v", got["error"])
	}
}

// TestVideoFailedCarriesError 运行期失败必须带 {code,message}，
// 不能降级成一句 "task failed" —— 异步错误是调用方唯一的解释。
func TestVideoFailedCarriesError(t *testing.T) {
	up := newVideoUpstream(t, 100)
	h := newVideoServer(t, up, nil)

	rec := postVideo(h, videoBody(""))
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	up.setStatus("FAILED", "")
	got := getVideo(t, h, created.ID)
	if got["status"] != "failed" {
		t.Fatalf("应 failed，得到 %v", got["status"])
	}
	e, _ := got["error"].(map[string]any)
	if e == nil || e["code"] == "" || e["message"] == "" {
		t.Fatalf("失败必须带 code 与 message: %v", got["error"])
	}
}

// TestVideoIdempotentSameIntent 同一份意图重复提交只产生一个任务。
//
// POST 超时后重试 POST 会造出**两个计费任务**，而客户端只认得第二个。
// 指纹相同 ⇒ 复用未终态任务。
func TestVideoIdempotentSameIntent(t *testing.T) {
	up := newVideoUpstream(t, 100)
	h := newVideoServer(t, up, nil)

	r1 := postVideo(h, videoBody(""))
	r2 := postVideo(h, videoBody(""))
	if r1.Code != http.StatusOK || r2.Code != http.StatusOK {
		t.Fatalf("两次都应 200: %d / %d", r1.Code, r2.Code)
	}
	if n := up.submitCount(); n != 1 {
		t.Fatalf("同意图只该提交 1 次，实际 %d 次（重复计费）", n)
	}
	if r1.Body.String() != r2.Body.String() {
		t.Fatalf("两次应返回同一个 id: %s vs %s", r1.Body.String(), r2.Body.String())
	}
}

// TestVideoParamsNotDoubleCarried 参数双载体不能同时下发。
//
// body 与 prompt 后缀都写 duration ⇒ 400。两者优先级不明，
// 而且后缀会被模型当提示词内容读进画面。
func TestVideoParamsNotDoubleCarried(t *testing.T) {
	up := newVideoUpstream(t, 100)
	h := newVideoServer(t, up, nil)

	rec := postVideo(h,
		`{"model":"m","content":[{"type":"text","text":"猫 --dur 10"}],"resolution":"720p","duration":5}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("双载体应 400，得到 %d: %s", rec.Code, rec.Body.String())
	}
	// 只有后缀（无 body 字段）时应被回填，且后缀从正文剥离。
	rec = postVideo(h,
		`{"model":"m","content":[{"type":"text","text":"猫 --dur 10"}],"resolution":"720p"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("仅后缀应被接受，得到 %d: %s", rec.Code, rec.Body.String())
	}
	up.mu.Lock()
	body := up.lastSubmit
	up.mu.Unlock()
	msg := firstMessageContent(body)
	if strings.Contains(msg, "--dur") {
		t.Fatalf("后缀必须从正文剥离（否则会被模型读进画面）: %q", msg)
	}
	if !strings.Contains(msg, "猫") {
		t.Fatalf("正文被吃掉了: %q", msg)
	}
}

// TestVideoRejectsUnprovenReference 未取证的参考素材要**明确拒绝**，不静默丢弃。
//
// 「未取证 ≠ 不支持」，但也不能假装支持 —— 静默丢弃会让调用方以为生效了，
// 生成的片子和预期不同而且已经计费。
func TestVideoRejectsUnprovenReference(t *testing.T) {
	up := newVideoUpstream(t, 100)
	h := newVideoServer(t, up, nil)

	rec := postVideo(h, `{"model":"m","resolution":"720p","duration":5,"content":[
		{"type":"text","text":"猫"},
		{"type":"image_url","image_url":{"url":"https://x/a.png"},"role":"first_frame"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未取证的首帧能力应 400，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "InvalidParameter.UnsupportedParameter") {
		t.Fatalf("应报 InvalidParameter.UnsupportedParameter: %s", rec.Body.String())
	}
	if up.submitCount() != 0 {
		t.Fatal("被拒绝的请求不该提交上游")
	}
}

// TestVideoListAndDelete 列表读本地快照；非终态不许「假装取消」。
func TestVideoListAndDelete(t *testing.T) {
	up := newVideoUpstream(t, 100)
	h := newVideoServer(t, up, nil)

	rec := postVideo(h, videoBody(""))
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	lrec := httptest.NewRecorder()
	h.ServeHTTP(lrec, httptest.NewRequest(http.MethodGet, videoTaskPath+"?page_size=10", nil))
	var list struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	if err := json.Unmarshal(lrec.Body.Bytes(), &list); err != nil {
		t.Fatalf("列表解析失败: %v", err)
	}
	if list.Total != 1 || len(list.Items) != 1 {
		t.Fatalf("列表应有 1 条: %+v", list)
	}

	// 非终态 DELETE：上游没有取消端点，必须明确拒绝而不是假装成功。
	drec := httptest.NewRecorder()
	h.ServeHTTP(drec, httptest.NewRequest(http.MethodDelete, videoTaskPath+"/"+created.ID, nil))
	if drec.Code != http.StatusBadRequest ||
		!strings.Contains(drec.Body.String(), "InvalidParameter.OperationDenied") {
		t.Fatalf("非终态 DELETE 应 400 OperationDenied，得到 %d: %s", drec.Code, drec.Body.String())
	}

	// 终态 DELETE：删本地记录。
	up.setStatus("SUCCESS", "https://cdn.example/v.mp4")
	getVideo(t, h, created.ID) // 刷新成终态
	drec = httptest.NewRecorder()
	h.ServeHTTP(drec, httptest.NewRequest(http.MethodDelete, videoTaskPath+"/"+created.ID, nil))
	if drec.Code != http.StatusOK {
		t.Fatalf("终态 DELETE 应 200，得到 %d: %s", drec.Code, drec.Body.String())
	}
	// 方舟 DELETE 的响应体是 `{"Result":{}}`，不是自定义的 {id, deleted}。
	var del struct {
		Result map[string]any `json:"Result"`
	}
	if err := json.Unmarshal(drec.Body.Bytes(), &del); err != nil || del.Result == nil {
		t.Fatalf("DELETE 应回 {\"Result\":{}}，得到 %s", drec.Body.String())
	}
}

// ---------- 小工具 ----------

func getVideo(t *testing.T, h http.Handler, id string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, videoTaskPath+"/"+id+"?refresh=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("查询 %s 失败 %d: %s", id, rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析任务对象失败: %v", err)
	}
	return out
}

// firstMessageContent 从上游提交体里取 messages[0].content。
func firstMessageContent(body map[string]any) string {
	if body == nil {
		return ""
	}
	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return ""
	}
	m, ok := msgs[0].(map[string]any)
	if !ok {
		return ""
	}
	s, _ := m["content"].(string)
	return s
}

// TestVideoRatioDomain 比例域是 6 档（前端 Jn 常量），不是 UI 上那 3 个。
//
// 「未测 ≠ 不支持」的活例子：4:3 / 3:4 一直可用，只是从来没人试过。
func TestVideoRatioDomain(t *testing.T) {
	// 余额与每日个数都给足：这条要连打 7 次，每次 720p/5s 扣 30 且占一个「今日视频」名额。
	up := newVideoUpstream(t, 100000)
	h := newVideoServer(t, up, func(c *config.Config) { c.Pool.MaxVideoPerDay = 0 })

	for _, ratio := range []string{"16:9", "4:3", "1:1", "3:4", "9:16"} {
		rec := postVideo(h, `{"model":"m","content":[{"type":"text","text":"猫"}],`+
			`"resolution":"720p","duration":5,"ratio":"`+ratio+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s 应被接受，得到 %d: %s", ratio, rec.Code, rec.Body.String())
		}
	}

	// adaptive 是 canonical 拼写，发上游要变成 origin。
	rec := postVideo(h, `{"model":"m","content":[{"type":"text","text":"猫"}],`+
		`"resolution":"720p","duration":5,"ratio":"adaptive"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("adaptive 应被接受，得到 %d: %s", rec.Code, rec.Body.String())
	}

	// 21:9 上游没有这个档位：必须 400，不许静默回落 16:9（那是另一条片子）。
	rec = postVideo(h, `{"model":"m","content":[{"type":"text","text":"猫"}],`+
		`"resolution":"720p","duration":5,"ratio":"21:9"}`)
	if rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "InvalidParameter.UnsupportedParameter") {
		t.Fatalf("21:9 应 400 InvalidParameter.UnsupportedParameter，得到 %d: %s", rec.Code, rec.Body.String())
	}
}

// TestVideoCapabilityByKernel 能力按内核分档：wan27 才有首尾帧，happyhorse 系才有参考图。
//
// 这条钉的是「枚举是矩阵」——同一个 content[] 换内核就从合法变非法。
func TestVideoCapabilityByKernel(t *testing.T) {
	up := newVideoUpstream(t, 100000)
	h := newVideoServer(t, up, func(c *config.Config) { c.Pool.MaxVideoPerDay = 0 })

	firstFrame := `{"type":"image_url","image_url":{"url":"https://x/a.png"},"role":"first_frame"}`

	// happyhorse11 不支持首尾帧。
	rec := postVideo(h, `{"model":"happyhorse11","resolution":"720p","duration":5,"content":[`+
		`{"type":"text","text":"猫"},`+firstFrame+`]}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "不支持首帧") {
		t.Fatalf("happyhorse11 首帧应 400，得到 %d: %s", rec.Code, rec.Body.String())
	}

	// wan27 支持首帧，且 materialId 链路已实测（2026-11-08）⇒ 放行。
	rec = postVideo(h, `{"model":"wan27","resolution":"720p","duration":5,"content":[`+
		`{"type":"text","text":"猫"},`+firstFrame+`]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("wan27 首帧应放行，得到 %d: %s", rec.Code, rec.Body.String())
	}

	// happyhorse 的参考图上限是 9 张。
	var refs []string
	for i := 0; i < 10; i++ {
		refs = append(refs, `{"type":"image_url","image_url":{"url":"https://x/`+itoa(i)+
			`.png"},"role":"reference_image"}`)
	}
	rec = postVideo(h, `{"model":"happyhorse11","resolution":"720p","duration":5,"content":[`+
		`{"type":"text","text":"猫"},`+strings.Join(refs, ",")+`]}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "最多 9 张") {
		t.Fatalf("10 张参考图应 400，得到 %d: %s", rec.Code, rec.Body.String())
	}

	// 参考视频 / 音频：上游 attachments 只接受 image。
	rec = postVideo(h, `{"model":"happyhorse11","resolution":"720p","duration":5,"content":[`+
		`{"type":"text","text":"猫"},{"type":"video_url","video_url":{"url":"https://x/a.mp4"},`+
		`"role":"reference_video"}]}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "参考视频") {
		t.Fatalf("参考视频应 400，得到 %d: %s", rec.Code, rec.Body.String())
	}
}

// TestVideoProbeUnlocksReference 打开实测开关后，wan27 首尾帧能真正下发 attachments。
//
// 断言的是**上游收到的 body**：genMode=vid_gen + attachments 两项（顺序=首帧,尾帧）。
// 若这里漏了 attachments，上游会当纯文生处理 —— 200 + 扣钱 + 出来一条无关的片子。
func TestVideoProbeUnlocksReference(t *testing.T) {
	old := videoMaterialEnabled
	videoMaterialEnabled = true
	t.Cleanup(func() { videoMaterialEnabled = old })

	up := newVideoUpstream(t, 100)
	cfg := config.Default()
	cfg.Video.Pricing = map[string]map[string]int{"720P": {"5": 30}}
	h := newVideoServer(t, up, func(c *config.Config) { c.Video.Pricing = cfg.Video.Pricing })

	rec := postVideo(h, `{"model":"wan27","resolution":"720p","duration":5,"content":[`+
		`{"type":"text","text":"猫"},`+
		`{"type":"image_url","image_url":{"url":"https://x/a.png"},"role":"first_frame"},`+
		`{"type":"image_url","image_url":{"url":"https://x/b.png"},"role":"last_frame"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("打开开关后应放行，得到 %d: %s", rec.Code, rec.Body.String())
	}

	up.mu.Lock()
	body := up.lastSubmit
	up.mu.Unlock()
	biz := bizDataOf(t, body)
	req, _ := biz["req"].(map[string]any)
	if req["genMode"] != "vid_gen" || req["rootModel"] != "wan27" {
		t.Fatalf("genMode/rootModel 不对: %v", req)
	}
	params, _ := req["params"].(map[string]any)
	arr, ok := params["attachments"].([]any)
	if !ok || len(arr) != 2 {
		t.Fatalf("首尾帧应有 2 项 attachments: %v", params["attachments"])
	}

	// 🔴 首尾帧的**唯一标识**是 extra.meta.videoGenerationMode，不是 genMode
	// （首尾帧与纯文生的 genMode 都是 vid_gen）。
	// 2026-11-08 实测踩过：漏了它，上游按纯文生处理 —— 红色首帧生效、蓝色尾帧被丢，
	// 5 秒产物全程纯红，而接口一路 200、积分照扣。
	meta := metaOfSubmit(t, body)
	if meta["videoGenerationMode"] != "first-last-frame" {
		t.Fatalf("首尾帧必须下发 videoGenerationMode=first-last-frame，实际 %v",
			meta["videoGenerationMode"])
	}
}

// metaOfSubmit 取上游提交体里的 messages[0].extra.meta。
func metaOfSubmit(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	msgs, _ := body["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("提交体没有 messages")
	}
	m, _ := msgs[0].(map[string]any)
	extra, _ := m["extra"].(map[string]any)
	meta, _ := extra["meta"].(map[string]any)
	return meta
}

// bizDataOf 从提交体里取出解析后的 biz_data。
func bizDataOf(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	if body == nil {
		t.Fatal("上游没收到提交体")
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("提交体没有 messages")
	}
	m, _ := msgs[0].(map[string]any)
	extra, _ := m["extra"].(map[string]any)
	meta, _ := extra["meta"].(map[string]any)
	raw, _ := meta["biz_data"].(string)
	if raw == "" {
		t.Fatalf("biz_data 缺失: %v", meta)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("biz_data 不是合法 JSON: %v", err)
	}
	return out
}

// TestVideoMaterialKillSwitch 环境开关关闭后，参考素材必须明确拒绝。
//
// 保留这个开关是为了「上游哪天改了素材体系」时能一键退回纯文生，
// 而不用回滚整个版本。退回时不能静默——必须告诉调用方现在只收纯文本。
func TestVideoMaterialKillSwitch(t *testing.T) {
	old := videoMaterialEnabled
	videoMaterialEnabled = false
	t.Cleanup(func() { videoMaterialEnabled = old })

	up := newVideoUpstream(t, 100000)
	h := newVideoServer(t, up, func(c *config.Config) { c.Pool.MaxVideoPerDay = 0 })

	rec := postVideo(h, `{"model":"wan27","resolution":"720p","duration":5,"content":[`+
		`{"type":"text","text":"猫"},`+
		`{"type":"image_url","image_url":{"url":"https://x/a.png"},"role":"first_frame"}]}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "DISABLE") {
		t.Fatalf("关闭开关后应 400 并点名开关，得到 %d: %s", rec.Code, rec.Body.String())
	}
	if up.submitCount() != 0 {
		t.Fatal("被拒绝的请求不该提交上游")
	}
}

// TestVideoUsageCarriesCredits usage 的 token 字段承载**积分**。
//
// Qwen 视频不统计 token（只扣积分），而方舟客户端读 completion_tokens 做计费。
// 这里钉住语义：token 字段 == 本次真扣的积分，且另给 credits 明文标注。
// 别让它被「优化」成编造的 token 数 —— 那会让对账永远对不上。
func TestVideoUsageCarriesCredits(t *testing.T) {
	up := newVideoUpstream(t, 1000)
	h := newVideoServer(t, up, nil)

	rec := postVideo(h, videoBody(""))
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)

	up.setStatus("SUCCESS", "https://cdn.example/v.mp4")
	got := getVideo(t, h, created.ID)

	usage, _ := got["usage"].(map[string]any)
	if usage == nil {
		t.Fatalf("终态任务应下发 usage: %v", got)
	}
	// 720p/5s = 30 积分。
	if usage["completion_tokens"] != float64(30) || usage["total_tokens"] != float64(30) {
		t.Fatalf("token 字段应为积分数 30（不是编造的 token）: %v", usage)
	}
	if usage["credits"] != float64(30) {
		t.Fatalf("credits 应显式给出 30: %v", usage)
	}
}

// TestVideoListFilterAndPaging 列表按方舟参数过滤分页。
//
// filter.model / filter.status / page_num / page_size 都走本地快照，
// 不打上游（方舟列表 QPS=1，拿它轮询会把池子打穿）。
func TestVideoListFilterAndPaging(t *testing.T) {
	up := newVideoUpstream(t, 100000)
	h := newVideoServer(t, up, func(c *config.Config) { c.Pool.MaxVideoPerDay = 0 })

	// 三个不同 prompt ⇒ 三个不同指纹 ⇒ 三个任务（幂等不会合并）。
	for _, p := range []string{"a", "b", "c"} {
		rec := postVideo(h, `{"model":"happyhorse11","content":[{"type":"text","text":"`+p+
			`"}],"resolution":"720p","duration":5}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("创建 %s 失败: %s", p, rec.Body.String())
		}
	}
	if rec := postVideo(h, `{"model":"wan27","content":[{"type":"text","text":"d"}],`+
		`"resolution":"720p","duration":5}`); rec.Code != http.StatusOK {
		t.Fatalf("wan27 创建失败: %s", rec.Body.String())
	}

	list := func(query string) (int, int) {
		lrec := httptest.NewRecorder()
		h.ServeHTTP(lrec, httptest.NewRequest(http.MethodGet, videoTaskPath+query, nil))
		var out struct {
			Items []map[string]any `json:"items"`
			Total int              `json:"total"`
		}
		if err := json.Unmarshal(lrec.Body.Bytes(), &out); err != nil {
			t.Fatalf("解析列表失败: %v（%s）", err, lrec.Body.String())
		}
		return out.Total, len(out.Items)
	}

	if total, n := list(""); total != 4 || n != 4 {
		t.Fatalf("全量应 4 条: total=%d page=%d", total, n)
	}
	if total, n := list("?filter.model=wan27"); total != 1 || n != 1 {
		t.Fatalf("filter.model=wan27 应 1 条: total=%d page=%d", total, n)
	}
	// 任务刚创建都是 queued，还没有终态。
	if total, _ := list("?filter.status=succeeded"); total != 0 {
		t.Fatalf("filter.status=succeeded 应 0 条，得到 %d", total)
	}
	// 分页：total 是过滤后的总数，不是本页条数。
	if total, n := list("?page_size=2&page_num=2"); total != 4 || n != 2 {
		t.Fatalf("分页: total=%d 第二页应 2 条，得到 %d", total, n)
	}
	if total, n := list("?page_size=2&page_num=9"); total != 4 || n != 0 {
		t.Fatalf("越界页: total=%d 应 0 条，得到 %d", total, n)
	}
}

// TestVideoAliasRoutesToKernel 配置别名只在路由层生效：
// 上游拿到的是内核，回显给客户端的仍是它传的原名。
//
// 这条钉的是 canonical 的「model 是路由名」：别名不是「假装支持别家的模型」，
// 而是「这个名字由哪个内核服务」。回显改名会让客户端下一轮找不到模型。
func TestVideoAliasRoutesToKernel(t *testing.T) {
	const alias = "doubao-seedance-2-0-pro-260628"
	up := newVideoUpstream(t, 100000)
	h := newVideoServer(t, up, func(c *config.Config) {
		c.Pool.MaxVideoPerDay = 0
		c.Video.Aliases = map[string]string{alias: "happyhorse11"}
	})

	rec := postVideo(h, `{"model":"`+alias+`","content":[{"type":"text","text":"猫"}],`+
		`"resolution":"720p","duration":5}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("别名应被接受，得到 %d: %s", rec.Code, rec.Body.String())
	}

	up.mu.Lock()
	body := up.lastSubmit
	up.mu.Unlock()
	biz := bizDataOf(t, body)
	req, _ := biz["req"].(map[string]any)
	if req["rootModel"] != "happyhorse11" {
		t.Fatalf("别名应路由到内核 happyhorse11，实际 %v", req["rootModel"])
	}

	// 回显必须是客户端原名。
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	got := getVideo(t, h, created.ID)
	if got["model"] != alias {
		t.Fatalf("model 应回显客户端原名 %q，实际 %v", alias, got["model"])
	}
}

// TestVideoModelsListAliases 模型清单要列出别名，并声明它不是 doubao 模型。
func TestVideoModelsListAliases(t *testing.T) {
	const alias = "doubao-seedance-2-0-pro-260628"
	up := newVideoUpstream(t, 100)
	h := newVideoServer(t, up, func(c *config.Config) {
		c.Video.Aliases = map[string]string{alias: "wan27"}
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	var out struct {
		Data []struct {
			ID       string `json:"id"`
			Media    string `json:"media"`
			Endpoint string `json:"endpoint"`
			Notes    string `json:"notes"`
			Params   struct {
				Modes []string `json:"modes"`
			} `json:"params"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析模型清单失败: %v", err)
	}

	var found bool
	for _, m := range out.Data {
		if m.ID != alias {
			continue
		}
		found = true
		if m.Media != "video" || m.Endpoint != videoTaskPath {
			t.Fatalf("别名条目的 media/endpoint 不对: %+v", m)
		}
		// wan27 的能力是 t2v + 首帧/首尾帧。
		if !strings.Contains(strings.Join(m.Params.Modes, ","), "first_last") {
			t.Fatalf("别名参数应跟内核能力走: %v", m.Params.Modes)
		}
		// 🔴 必须声明「不是 doubao 模型」，否则会被当成字节的报价单。
		if !strings.Contains(m.Notes, "不是 doubao") {
			t.Fatalf("别名条目要说明产物来源: %s", m.Notes)
		}
	}
	if !found {
		t.Fatalf("模型清单里没有别名 %q", alias)
	}
}

// bizAudio 从上游提交体里取 biz_data.req.params.audio。
func bizAudio(t *testing.T, body map[string]any) bool {
	t.Helper()
	req, _ := bizDataOf(t, body)["req"].(map[string]any)
	params, _ := req["params"].(map[string]any)
	b, _ := params["audio"].(bool)
	return b
}

// TestVideoAudioDefaultsToConfig 不传 generate_audio 时必须落 config.video.audio。
//
// 这条断言盯的是「配置有没有被消费」：旧实现是 `audio != nil && *audio`，
// 于是 config.video.audio=true 从来没生效过 —— 接口一路 200、积分照扣，
// 产物却静音，而面板上看起来「默认开」。
func TestVideoAudioDefaultsToConfig(t *testing.T) {
	up := newVideoUpstream(t, 100)
	h := newVideoServer(t, up, func(c *config.Config) { c.Video.Audio = true })

	rec := postVideo(h, videoBody(""))
	if rec.Code != http.StatusOK {
		t.Fatalf("应成功，得到 %d: %s", rec.Code, rec.Body.String())
	}

	up.mu.Lock()
	body := up.lastSubmit
	up.mu.Unlock()
	if got := bizAudio(t, body); !got {
		t.Fatalf("配置默认开且请求未带 generate_audio 时，上游应收到 audio=true，实际 %v", got)
	}
}

// TestVideoAudioExplicitFalseWins 显式 false 必须压过配置默认。
//
// 默认值只能填「没说的时候」，不能覆盖调用方的明确意图。
func TestVideoAudioExplicitFalseWins(t *testing.T) {
	up := newVideoUpstream(t, 100)
	h := newVideoServer(t, up, func(c *config.Config) { c.Video.Audio = true })

	rec := postVideo(h, videoBody(`"generate_audio":false`))
	if rec.Code != http.StatusOK {
		t.Fatalf("应成功，得到 %d: %s", rec.Code, rec.Body.String())
	}

	up.mu.Lock()
	body := up.lastSubmit
	up.mu.Unlock()
	if got := bizAudio(t, body); got {
		t.Fatal("显式 generate_audio=false 应压过配置默认，实际上游收到了 audio=true")
	}
}
