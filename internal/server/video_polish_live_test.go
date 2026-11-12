package server

// 后处理整链路的连通性测试（默认跳过）。
//
// 跑法：
//   POLISH_LIVE=1 WM_SAMPLE=/path/to/watermarked.mp4 \
//   S3_ENDPOINT=https://oss.s3ai.cn S3_BUCKET=cdn S3_AK=... S3_SK=... \
//   go test ./internal/server/ -run TestPolishPipelineLive -v
//
// 会真的往对象存储写一个 qwen2api-live-<ts>.mp4，测试结束不删除
// （留给人工抽查画面），需要的话手工清理。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/config"
)

func TestPolishPipelineLive(t *testing.T) {
	sample := os.Getenv("WM_SAMPLE")
	if os.Getenv("POLISH_LIVE") != "1" || sample == "" {
		t.Skip("设置 POLISH_LIVE=1 与 WM_SAMPLE=<样例 mp4> 才跑")
	}
	st := config.Storage{
		Enabled:        true,
		Endpoint:       os.Getenv("S3_ENDPOINT"),
		Bucket:         os.Getenv("S3_BUCKET"),
		AccessKey:      os.Getenv("S3_AK"),
		SecretKey:      os.Getenv("S3_SK"),
		Prefix:         "qwen2api-live",
		StripWatermark: true,
		Quality:        "near",
		Workers:        1,
		TimeoutS:       300,
		FFmpegPath:     "ffmpeg",
	}
	if st.Endpoint == "" {
		t.Skip("缺少 S3_ENDPOINT")
	}

	data, err := os.ReadFile(sample)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	cfg := config.Config{Storage: st}
	s := &Server{}
	s.cfg.Store(cfg)
	p := &videoPolisher{s: s}

	rec := &videoRecord{ID: "qwen2api-live-" + time.Now().Format("20060102T150405")}
	rec.UpstreamURL = srv.URL + "/v.mp4"

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	url, err := p.run(ctx, rec, st)
	if err != nil {
		t.Fatalf("后处理失败: %v", err)
	}
	t.Logf("已上传: %s", url)

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("回读 HTTP %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) < 10000 {
		t.Fatalf("回读内容过小: %d 字节", len(body))
	}
	t.Logf("回读 %d 字节，原始 %d 字节", len(body), len(data))
}
