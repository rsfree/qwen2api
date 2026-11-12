package server

// 图片去水印 + 托管的连通性测试（默认跳过，避免 CI 出网）。
//
// 跑法：
//   IMG_LIVE=1 WM_IMG=/path/to/watermarked.png \
//   S3_ENDPOINT=https://oss.s3ai.cn S3_BUCKET=cdn S3_AK=... S3_SK=... \
//   go test ./internal/server/ -run TestPolishImageLive -v

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

func TestPolishImageLive(t *testing.T) {
	sample := os.Getenv("WM_IMG")
	if os.Getenv("IMG_LIVE") != "1" || sample == "" {
		t.Skip("设置 IMG_LIVE=1 与 WM_IMG=<带水印的图片> 才跑")
	}
	enabled := true
	st := config.Storage{
		Enabled:        &enabled,
		Endpoint:       os.Getenv("S3_ENDPOINT"),
		Bucket:         os.Getenv("S3_BUCKET"),
		AccessKey:      os.Getenv("S3_AK"),
		SecretKey:      os.Getenv("S3_SK"),
		Prefix:         "",
		StripWatermark: true,
		Quality:        "near",
	}
	if st.Endpoint == "" {
		t.Skip("缺少 S3_ENDPOINT")
	}

	data, err := os.ReadFile(sample)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	s := &Server{}
	s.cfg.Store(config.Config{Storage: st})

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	url, png, err := s.polishImage(ctx, srv.URL+"/a.png")
	if err != nil {
		t.Fatalf("去水印/托管失败: %v", err)
	}
	if len(png) < 1000 {
		t.Fatalf("处理结果过小: %d 字节", len(png))
	}
	t.Logf("已上传: %s（%d 字节，源 %d 字节）", url, len(png), len(data))

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("回读失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("回读 HTTP %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	t.Logf("回读 %d 字节", len(body))
}
