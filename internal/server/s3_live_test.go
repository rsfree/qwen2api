package server

// SigV4 上传的连通性测试（默认跳过，避免 CI 出网）。
//
// 跑法：
//   S3_LIVE=1 go test ./internal/server/ -run TestS3PutLive -v
//
// 只在配置了真实 endpoint 时执行；对象键带时间戳，测完不影响业务前缀。

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/yuanjie-ai/qwen2api/internal/config"
)

func TestS3PutLive(t *testing.T) {
	if os.Getenv("S3_LIVE") != "1" {
		t.Skip("设置 S3_LIVE=1 才跑（需要真实对象存储）")
	}
	sc := config.Storage{
		Endpoint:  os.Getenv("S3_ENDPOINT"),
		Bucket:    os.Getenv("S3_BUCKET"),
		AccessKey: os.Getenv("S3_AK"),
		SecretKey: os.Getenv("S3_SK"),
		Prefix:    "_probe",
	}
	if sc.Endpoint == "" || sc.Bucket == "" {
		t.Skip("缺少 S3_ENDPOINT / S3_BUCKET")
	}
	key := "sigv4-" + time.Now().UTC().Format("20060102T150405") + ".txt"
	payload := []byte("qwen2api sigv4 probe")

	got, err := s3PutObject(context.Background(), sc, key, payload, "text/plain")
	if err != nil {
		t.Fatalf("上传失败: %v", err)
	}
	t.Logf("已上传: %s", got)
	if _, err := os.Stat("/dev/null"); err != nil {
		t.Log("noop")
	}
}
