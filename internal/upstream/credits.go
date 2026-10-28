package upstream

// 积分（Credits）余额。
//
// 🔴 这是**第三个域名**：chat.qwen.ai 管对话、auth.qwen.ai 管登录态、
// sg-entitlement.qwen.com 管积分。2026-10-29 用页面自己的 XHR 抓到的：
//
//	GET /api/entitlement/credits/credits/history?biz_id=ai_qwen&…&pageNum=1&pageSize=20
//	→ data: {totalCredits: 280, subscriptionCredits: 270, bonusCredits: 10,
//	         total: 32, usageList: [{title:"credits.dec.HappyHorse1.1.720P.5s.video",
//	                                 amount: 30, date: 1793209700000, state:"-"}]}
//
// 同一份响应里既有**余额**又有**消费明细**，所以只要一个请求就能同时做两件事：
// 选号前判够不够、事后核对单价。
//
// 📌 `api/v2/credits/pricing`（chat 域）是另一回事：它给的是**价格表**，
// 而且按模型版本分两套（hh11* / hh*），不含余额。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Credits 账号当前积分余额。
type Credits struct {
	// Total 可用总额（= 订阅 + 奖励）。判够不够只看这个。
	Total int `json:"totalCredits"`
	// Subscription 订阅额度（会员套餐给的）。
	Subscription int `json:"subscriptionCredits"`
	// Bonus 奖励额度（每日免费积分等）。
	Bonus int `json:"bonusCredits"`
}

// CreditRecord 一笔积分流水。
type CreditRecord struct {
	ID     string `json:"id"`
	Title  string `json:"title"`  // 如 credits.dec.HappyHorse1.1.720P.5s.video
	Amount int    `json:"amount"` // 正数=入账，state="-" 表示扣费
	State  string `json:"state"`
	Date   int64  `json:"date"` // 毫秒时间戳
	Unit   string `json:"unit"`
}

// CreditHistory 余额 + 最近流水。
type CreditHistory struct {
	Credits
	TotalCount int            `json:"total"`
	UsageList  []CreditRecord `json:"usageList"`
}

// Credits 读账号积分余额。
//
// 请求成本很低（pageSize=1 只取余额字段），但仍然建议调用方做缓存 ——
// 视频选号不该每次都打一次三方域名。
func (c *Client) Credits(ctx context.Context, cred string) (*Credits, error) {
	h, err := c.creditHistory(ctx, cred, 1)
	if err != nil {
		return nil, err
	}
	return &h.Credits, nil
}

// CreditHistory 读余额 + 最近 n 条流水（排查单价用）。
func (c *Client) CreditHistory(ctx context.Context, cred string, pageSize int) (*CreditHistory, error) {
	return c.creditHistory(ctx, cred, pageSize)
}

func (c *Client) creditHistory(ctx context.Context, cred string, pageSize int) (*CreditHistory, error) {
	if pageSize <= 0 {
		pageSize = 1
	}
	base := strings.TrimRight(c.cfg.EntitlementBaseURL, "/")
	if base == "" {
		// 没配就不猜：宁可上层走"余额未知"分支，也别拼一个错的域名。
		return nil, NewErr(KindUnknown, "credits", 0, "",
			"未配置 upstream.entitlement_base_url（积分域，默认 sg-entitlement.qwen.com）", false)
	}

	q := url.Values{}
	// 这几个参数是页面原样带的；少了 biz_id 会 404（2026-10-29 实测）。
	q.Set("biz_id", "ai_qwen")
	q.Set("chat_client", "h5")
	q.Set("device", "pc")
	q.Set("fr", "pc")
	q.Set("pr", "qwen-i18n")
	q.Set("la", "zh-CN")
	q.Set("tz", AsiaTimezone)
	q.Set("wv", c.cfg.Version)
	q.Set("pageNum", "1")
	q.Set("pageSize", strconv.Itoa(pageSize))

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	u := base + "/api/entitlement/credits/credits/history?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	// 跨站端点：Sec-Fetch-Site / x-request-origin 与业务域不同（同 auth 域的处理）。
	req.Header = c.buildHeadersFor(cred, c.cfg.BaseURL+"/settings/usage-billing", true)
	req.Header.Del("Content-Type") // GET 不带体，页面也不发

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, transportErr("credits", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, transportErr("credits 读响应", err)
	}

	// 这个域的信封与 chat 域不同：{traceId, httpCode, success, errorCode, errorMsg, data}。
	// 所以不复用 decodeEnvelope —— 它的错误字段名（code/details）在这边是 errorCode/errorMsg。
	var env struct {
		HTTPCode  int            `json:"httpCode"`
		Success   *bool          `json:"success"`
		ErrorMsg  string         `json:"errorMsg"`
		ErrorCode string         `json:"errorCode"`
		Data      *CreditHistory `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, NewErr(KindUnknown, "credits", resp.StatusCode, "",
			"响应不是 JSON: "+truncate(string(raw), 200), false)
	}
	if resp.StatusCode >= 400 || env.HTTPCode >= 400 {
		// 走统一的 HTTP 归类（401→auth、429→rate_limit…），别自造一份。
		return nil, classifyHTTP(resp.StatusCode, resp.StatusCode, "credits", raw)
	}
	if env.Success != nil && !*env.Success {
		return nil, NewErr(KindUnknown, "credits", resp.StatusCode, env.ErrorCode,
			"积分接口返回失败: "+env.ErrorMsg, false)
	}
	if env.Data == nil {
		return nil, NewErr(KindUnknown, "credits", resp.StatusCode, "",
			"积分接口没有 data（响应形态可能变了）", false)
	}
	return env.Data, nil
}

// AsiaTimezone 页面用的时区参数值。
const AsiaTimezone = "Asia/Shanghai"
