package panel

// 账号列表的服务端分页。
//
// 为什么必须放服务端：3000+ 账号时 overview 的 accounts 数组有 840KB，
// 面板轮询一次要好几秒（跨洋实测 5s），而界面上一次只看 20~50 行。
// 现在 overview 只带汇总（几百字节），列表按页取。

import (
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/yuanjie-ai/qwen2api/internal/pool"
)

// acctStats 账号池汇总（面板顶部的统计卡与筛选 chip 用）。
type acctStats struct {
	Total        int `json:"total"`
	Ready        int `json:"ready"`
	Cooling      int `json:"cooling"`
	Dead         int `json:"dead"`
	Exhausted    int `json:"exhausted"`
	ImgLeft      int `json:"img_left"`
	Credits      int `json:"credits"`
	CreditsKnown int `json:"credits_known"`
	VidLeft      int `json:"vid_left"`
	RefreshKnown int `json:"refresh_known"`
	// TokenFresh 当前 access token 仍未过期的账号数（看保活是否在跑）。
	TokenFresh int `json:"token_fresh"`
	// RefreshExpiring refresh_token 剩余不足 3 天、待保养的账号数。
	RefreshExpiring int `json:"refresh_expiring"`
}

// acctStatusOf 与前端 acctStatus 同口径：额度用尽算独立状态。
//
// 🔴 余额"未知"不算用尽：未知时网关本身也不会拦（交给上游兜底），
// 面板先判成用尽会让人误以为这个号废了。
func acctStatusOf(v pool.View, videoMin int) string {
	switch v.State {
	case "auth_dead":
		return "dead"
	case "cooling":
		return "cooling"
	}
	imgFull := v.ImageLimit > 0 && v.ImageToday >= v.ImageLimit
	noCredit := v.CreditKnown && v.CreditBalance < videoMin
	if imgFull && noCredit {
		return "exhausted"
	}
	return "ready"
}

// summarize 汇总整池（不含分页）。
func summarize(views []pool.View, videoMin int) acctStats {
	st := acctStats{Total: len(views)}
	for _, v := range views {
		switch acctStatusOf(v, videoMin) {
		case "dead":
			st.Dead++
		case "cooling":
			st.Cooling++
		case "exhausted":
			st.Exhausted++
		default:
			st.Ready++
		}
		if v.ImageLimit > 0 {
			if left := v.ImageLimit - v.ImageToday; left > 0 {
				st.ImgLeft += left
			}
		}
		if v.CreditKnown {
			st.Credits += v.CreditBalance
			st.CreditsKnown++
			if videoMin > 0 && v.CreditBalance > 0 {
				st.VidLeft += v.CreditBalance / videoMin
			}
		}
		if v.HasRefresh {
			st.RefreshKnown++
		}
		if v.TokenLeftS > 0 {
			st.TokenFresh++
		}
		if v.RefreshKnown && v.RefreshLeftS < 3*86400 {
			st.RefreshExpiring++
		}
	}
	return st
}

// handleAccounts GET /panel/api/accounts?page=1&size=50&q=&state=&sort=
func (p *Panel) handleAccounts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page := atoiOr(q.Get("page"), 1)
	if page < 1 {
		page = 1
	}
	size := atoiOr(q.Get("size"), 50)
	if size < 1 {
		size = 50
	}
	if size > 500 {
		size = 500
	}
	search := strings.ToLower(strings.TrimSpace(q.Get("q")))
	state := strings.TrimSpace(q.Get("state"))
	sortKey := strings.TrimSpace(q.Get("sort"))

	views := p.b.Pool().Snapshot()
	// 容量折算用"最便宜的 720P 5s"单价；查不到就按 30（本仓默认表）。
	videoMin := videoMinDefault
	if c, ok := p.b.Config().Video.VideoCredits("720P", 5); ok && c > 0 {
		videoMin = c
	}
	st := summarize(views, videoMin)

	// 过滤
	filtered := make([]pool.View, 0, len(views))
	for _, v := range views {
		if search != "" {
			hit := strings.Contains(strings.ToLower(v.Name), search) ||
				strings.Contains(strings.ToLower(v.Email), search) ||
				strings.Contains(strings.ToLower(v.Note), search)
			if !hit {
				continue
			}
		}
		if state != "" && state != "all" && acctStatusOf(v, videoMin) != state {
			continue
		}
		filtered = append(filtered, v)
	}

	// 排序（与前端原来的选项一一对应）
	bal := func(v pool.View) int {
		if v.CreditKnown {
			return v.CreditBalance
		}
		return -1
	}
	rank := map[string]int{"dead": 0, "cooling": 1, "exhausted": 2, "ready": 3}
	sort.SliceStable(filtered, func(i, j int) bool {
		a, b := filtered[i], filtered[j]
		switch sortKey {
		case "balance_desc":
			return bal(a) > bal(b)
		case "balance_asc":
			return bal(a) < bal(b)
		case "fails_desc":
			return a.Failures > b.Failures
		case "served_desc":
			return a.Served > b.Served
		case "name":
			return a.Name < b.Name
		}
		ra, rb := rank[acctStatusOf(a, videoMin)], rank[acctStatusOf(b, videoMin)]
		if ra != rb {
			return ra < rb
		}
		return bal(a) > bal(b)
	})

	total := len(filtered)
	pages := (total + size - 1) / size
	if pages < 1 {
		pages = 1
	}
	if page > pages {
		page = pages
	}
	start := (page - 1) * size
	end := start + size
	if start > total {
		start = total
	}
	if end > total {
		end = total
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"items": filtered[start:end],
		"total": total,
		"page":  page,
		"size":  size,
		"pages": pages,
		"stats": st,
	})
}

const videoMinDefault = 30

func atoiOr(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
