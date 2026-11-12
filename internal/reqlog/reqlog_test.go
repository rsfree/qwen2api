package reqlog

import (
	"bytes"
	"strings"
	"testing"
)

// 首字分位只统计"真的出了字"的请求：失败/无首字的样本混进来会把分位拉低。
func TestStatsTTFTIgnoresEmptySamples(t *testing.T) {
	l := New(10)
	l.Echo = false
	// 100 / 200 / 300 / 400，另加一条失败（FirstByteMS=0）不该被计入。
	for _, v := range []int64{100, 200, 300, 400} {
		l.Add(Entry{Status: 200, FirstByteMS: v, FirstTextMS: v - 50})
	}
	l.Add(Entry{Status: 502, Error: "boom"})

	st := l.Stats()
	if st.TTFT.N != 4 {
		t.Fatalf("首字样本数应为 4，得到 %d", st.TTFT.N)
	}
	if st.TTFT.P50 != 300 {
		t.Fatalf("P50 应为 300，得到 %d", st.TTFT.P50)
	}
	if st.TTFT.Max != 400 {
		t.Fatalf("Max 应为 400，得到 %d", st.TTFT.Max)
	}
	if st.UpstreamTTFT.N != 4 || st.UpstreamTTFT.P50 != 250 {
		t.Fatalf("上游首字 P50 应为 250，得到 %d (n=%d)", st.UpstreamTTFT.P50, st.UpstreamTTFT.N)
	}
}

// 环形写满后统计不能把"上一轮的残留"算进来。
func TestStatsTTFTAfterWrap(t *testing.T) {
	l := New(2)
	l.Echo = false
	l.Add(Entry{Status: 200, FirstByteMS: 999})
	l.Add(Entry{Status: 200, FirstByteMS: 10})
	l.Add(Entry{Status: 200, FirstByteMS: 20}) // 覆盖第一条
	st := l.Stats()
	if st.TTFT.N != 2 {
		t.Fatalf("环形写满后样本数应为 2，得到 %d", st.TTFT.N)
	}
	if st.TTFT.Max != 20 {
		t.Fatalf("被覆盖的 999 不该计入，Max 得到 %d", st.TTFT.Max)
	}
}

// Agent 的工具轨迹必须出现在流水行里 —— 否则"这 11 秒干了什么"只剩耗时。
func TestEchoIncludesToolTrace(t *testing.T) {
	var buf bytes.Buffer
	l := New(4)
	l.Out = &buf
	l.Add(Entry{Status: 200, Tools: "bash(echo 42) → completed exit=0 stdout=42"})
	line := buf.String()
	if !strings.Contains(line, "tools=bash(echo 42)") {
		t.Fatalf("流水行没带工具轨迹: %s", line)
	}
	if !strings.Contains(line, "exit=0") {
		t.Fatalf("工具结果被丢了: %s", line)
	}
}

// 多轮工具会把轨迹撑爆：超长必须截断，且按 rune 截（不能切出半个汉字）。
func TestEchoClipsLongToolTrace(t *testing.T) {
	var buf bytes.Buffer
	l := New(4)
	l.Out = &buf
	long := strings.Repeat("工具调用", 100)
	l.Add(Entry{Status: 200, Tools: long})
	line := buf.String()
	if !strings.Contains(line, "…") {
		t.Fatalf("超长轨迹未被截断: %s", line)
	}
	if strings.Count(line, "工") > 210 {
		t.Fatalf("截断阈值失效: %d 个字符", strings.Count(line, "工"))
	}
	if !strings.Contains(line, "工具") {
		t.Fatalf("截断切坏了中文: %s", line)
	}
}

// 缓存看板的口径：token 维度 + 估算单列 + 失败请求不进分母。
func TestStatsCache(t *testing.T) {
	l := New(10)
	l.Echo = false

	// 上游报的真值
	l.Add(Entry{Model: "m1", Status: 200, PromptTokens: 1000, CachedTokens: 800})
	// 上游没报、我们估的
	l.Add(Entry{Model: "m1", Status: 200, PromptTokens: 500, CachedTokens: 300, CacheEst: true})
	// 完全没命中
	l.Add(Entry{Model: "m2", Status: 200, PromptTokens: 200})
	// 失败请求：PromptTokens=0，不能进分母（否则命中率虚高）
	l.Add(Entry{Model: "m2", Status: 502})

	s := l.Stats().Cache
	if s.PromptTokens != 1700 || s.CachedTokens != 1100 {
		t.Fatalf("累计应为 1700/1100，得到 %d/%d", s.PromptTokens, s.CachedTokens)
	}
	if s.Reqs != 3 {
		t.Fatalf("只应统计 3 个带 prompt_tokens 的请求，得到 %d", s.Reqs)
	}
	if s.EstimatedTokens != 300 {
		t.Fatalf("估算部分应为 300，得到 %d", s.EstimatedTokens)
	}
	if got := s.HitPct; got < 64.7 || got > 64.8 {
		t.Fatalf("命中率应为 64.71%%，得到 %.2f", got)
	}

	m1 := s.ByModel["m1"]
	if m1.Reqs != 2 || m1.PromptTokens != 1500 || m1.CachedTokens != 1100 {
		t.Fatalf("m1 聚合不对: %+v", m1)
	}
	m2 := s.ByModel["m2"]
	if m2.Reqs != 1 || m2.CachedTokens != 0 || m2.HitPct != 0 {
		t.Fatalf("m2 聚合不对: %+v", m2)
	}
}
