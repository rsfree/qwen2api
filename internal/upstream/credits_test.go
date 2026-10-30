package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yuanjie-ai/qwen2api/internal/config"
)

// 真实响应形态（2026-10-29 抓自 sg-entitlement.qwen.com）：
// 信封是 {traceId, httpCode, success, errorCode, errorMsg, data}，
// 与 chat 域的 {success, request_id, data} **不是同一个**。
const creditsBody = `{
 "traceId":"21018f4817932105525011641e183f",
 "httpCode":200,
 "success":true,
 "errorCode":null,
 "errorMsg":null,
 "data":{
   "totalCredits":280,
   "subscriptionCredits":270,
   "bonusCredits":10,
   "pageNum":1,"pageSize":1,"total":32,
   "usageList":[{"id":"217970435829026816",
     "title":"credits.dec.HappyHorse1.1.720P.5s.video",
     "date":1793209700000,"unit":"Credits","amount":30,"state":"-"}]
 }
}`

func newCreditsClient(t *testing.T, body string, status int) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/api/entitlement/credits/credits/history") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)

	cfg := config.Default().Upstream
	cfg.EntitlementBaseURL = srv.URL
	cfg.BaseURL = srv.URL // 让 referer 拼得出来
	return New(cfg), srv
}

func TestCreditsParsesBalance(t *testing.T) {
	cli, _ := newCreditsClient(t, creditsBody, http.StatusOK)
	c, err := cli.Credits(context.Background(), "tok")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if c.Total != 280 || c.Subscription != 270 || c.Bonus != 10 {
		t.Fatalf("余额字段不对: %+v", c)
	}
}

func TestCreditsSendsRequiredQuery(t *testing.T) {
	// biz_id 少了直接 404（实测）。这几个参数是页面原样带的，别省。
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(creditsBody))
	}))
	defer srv.Close()
	cfg := config.Default().Upstream
	cfg.EntitlementBaseURL = srv.URL
	cli := New(cfg)

	if _, err := cli.Credits(context.Background(), "tok"); err != nil {
		t.Fatal(err)
	}
	for _, kv := range []string{"biz_id=ai_qwen", "pageNum=1", "pageSize=1", "wv="} {
		if !strings.Contains(gotQuery, kv) {
			t.Fatalf("查询参数缺 %s：%s", kv, gotQuery)
		}
	}
}

func TestCreditsBusinessFailureIsError(t *testing.T) {
	// success:false 必须当错误（否则会把 0 当余额，把所有请求拦死）。
	cli, _ := newCreditsClient(t,
		`{"httpCode":200,"success":false,"errorCode":"Forbidden","errorMsg":"无权访问","data":null}`,
		http.StatusOK)
	if _, err := cli.Credits(context.Background(), "tok"); err == nil {
		t.Fatal("success:false 应报错")
	}
}

func TestCreditsMissingDataIsError(t *testing.T) {
	// data 缺失 ⇒ 不能当 0 余额。
	cli, _ := newCreditsClient(t, `{"httpCode":200,"success":true,"data":null}`, http.StatusOK)
	if _, err := cli.Credits(context.Background(), "tok"); err == nil {
		t.Fatal("data 为空应报错")
	}
}

func TestCreditsNoBaseURLDoesNotGuess(t *testing.T) {
	// 没配积分域时不能瞎拼一个域名去请求。
	cfg := config.Default().Upstream
	cfg.EntitlementBaseURL = ""
	cli := New(cfg)
	_, err := cli.Credits(context.Background(), "tok")
	if err == nil {
		t.Fatal("未配置积分域应报错")
	}
	if !strings.Contains(err.Error(), "entitlement_base_url") {
		t.Fatalf("错误应指向配置项: %v", err)
	}
}

func TestCreditHistoryKeepsRecords(t *testing.T) {
	cli, _ := newCreditsClient(t, creditsBody, http.StatusOK)
	h, err := cli.CreditHistory(context.Background(), "tok", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.UsageList) != 1 {
		t.Fatalf("流水条数不对: %d", len(h.UsageList))
	}
	r := h.UsageList[0]
	if r.Amount != 30 || !strings.Contains(r.Title, "HappyHorse1.1") {
		t.Fatalf("流水字段不对: %+v", r)
	}
}

// 每日赠送时刻从流水里推：只认 title 含 daily 的，取最大时间；毫秒→秒。
func TestDeriveDailyAt(t *testing.T) {
	list := []CreditRecord{
		{Title: "credits.dec.HappyHorse1.1.720P.5s.video", Amount: 30, Date: 1793400000000, State: "-"},
		{Title: "credits.init.title.daily", Amount: 40, Date: 1793324947000, State: "+"},
		{Title: "credits.init.title.daily", Amount: 40, Date: 1793238547000, State: "+"}, // 前一天
		{Title: "credits.inc.subscription", Amount: 270, Date: 1793500000000, State: "+"},
	}
	if got, want := deriveDailyAt(list), int64(1793324947000/1000); got != want {
		t.Fatalf("daily 时刻不对: got %d want %d", got, want)
	}
	if got := deriveDailyAt(nil); got != 0 {
		t.Fatalf("空流水应为 0，得到 %d", got)
	}
	if got := deriveDailyAt([]CreditRecord{{Title: "credits.dec.video", Date: 1}}); got != 0 {
		t.Fatalf("没有 daily 应为 0，得到 %d", got)
	}
}
