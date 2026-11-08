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
