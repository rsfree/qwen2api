package upstream

import (
	"testing"
	"time"
)

// 基准签名由 Python（同一套 HMAC 派生）独立算出，用来钉住 Go 实现。
// 签错的表现是 OSS 403 AccessDenied，从错误信息看不出是"哪一步算错了"，只能靠这条测试。
func TestSignOSSV4Golden(t *testing.T) {
	sts := &STSToken{
		AccessKeyID:     "test-ak",
		AccessKeySecret: "test-secret-key",
		SecurityToken:   "test-sts",
		BucketName:      "my-bucket",
		Endpoint:        "oss-cn-hangzhou.aliyuncs.com",
		FilePath:        "user/a.png",
		Region:          "cn-hangzhou",
	}
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	h := signOSSV4(sts, "/my-bucket/user/a.png", "image/png", "cn-hangzhou", now)

	want := "OSS4-HMAC-SHA256 Credential=test-ak/20261028/cn-hangzhou/oss/aliyun_v4_request," +
		"Signature=3d74692259037e3633f54db4d4ac3484295428add33673891dce1f0982aae518"
	if h["Authorization"] != want {
		t.Fatalf("签名不匹配\n got: %s\nwant: %s", h["Authorization"], want)
	}
	if h["x-oss-date"] != "20261028T120000Z" {
		t.Fatalf("x-oss-date 格式不对: %q", h["x-oss-date"])
	}
	if h["x-oss-content-sha256"] != "UNSIGNED-PAYLOAD" {
		t.Fatalf("payload hash 不对: %q", h["x-oss-content-sha256"])
	}
	// STS 临时凭证必须进签名头，否则 OSS 直接 403。
	if h["x-oss-security-token"] != "test-sts" {
		t.Fatalf("security-token 未签名: %q", h["x-oss-security-token"])
	}
}

func TestOSSTargetStripsPrefixes(t *testing.T) {
	sts := &STSToken{BucketName: "b", Endpoint: "https://oss-cn-beijing.aliyuncs.com/",
		Region: "oss-cn-beijing"}
	host, region := ossTarget(sts)
	if host != "b.oss-cn-beijing.aliyuncs.com" {
		t.Fatalf("host 组装不对: %q", host)
	}
	// region 的 oss- 前缀不剥会签名 region 错误（对加速端点必现）。
	if region != "cn-beijing" {
		t.Fatalf("region 未剥前缀: %q", region)
	}
	// endpoint 已含 bucket 时不能重复拼。
	sts2 := &STSToken{BucketName: "b", Endpoint: "b.oss-cn-beijing.aliyuncs.com"}
	if host2, _ := ossTarget(sts2); host2 != "b.oss-cn-beijing.aliyuncs.com" {
		t.Fatalf("重复拼接 bucket: %q", host2)
	}
}

func TestFileTypeOf(t *testing.T) {
	cases := map[string]string{
		"image/png":       "image",
		"video/mp4":       "video",
		"audio/mpeg":      "audio",
		"application/pdf": "file",
		"":                "file",
	}
	for in, want := range cases {
		if got := fileTypeOf(in); got != want {
			t.Fatalf("fileTypeOf(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestBuildFileEntryShape(t *testing.T) {
	sts := &STSToken{FileID: "fid-1", FileURL: "https://x/y.png", FilePath: "p"}
	sts.size = 1234
	e := buildFileEntry("image", "a.png", "image/png", sts)

	// 这些键缺任何一个，上游都会把附件当"没传完"静默忽略。
	for _, k := range []string{"type", "file", "id", "url", "name", "collection_name",
		"progress", "status", "greenNet", "size", "error", "itemId", "file_type",
		"showType", "file_class"} {
		if _, ok := e[k]; !ok {
			t.Fatalf("files[] 条目缺字段 %q", k)
		}
	}
	if e["status"] != "uploaded" || e["greenNet"] != "success" {
		t.Fatalf("上传状态字段不对: %v / %v", e["status"], e["greenNet"])
	}
	if e["showType"] != "image" || e["file_class"] != "vision" {
		t.Fatalf("image 类映射不对: %v / %v", e["showType"], e["file_class"])
	}
	if e["size"] != int64(1234) {
		t.Fatalf("size 丢失: %v", e["size"])
	}
}

func TestImageEditSpecFields(t *testing.T) {
	files := []FileRef{{"id": "f1"}, {"id": "f2"}}
	body := BuildSubmitSpec("chat-1", "", "qwen3.7-plus", "改成水彩", "fast",
		ImageEditSpec(files), mustTime())

	msg := body.Messages[0]
	if msg.ChatType != "image_edit" || msg.SubChatType != "image_edit" {
		t.Fatalf("chat_type 不对: %q/%q", msg.ChatType, msg.SubChatType)
	}
	if len(msg.Files) != 2 {
		t.Fatalf("files 数量不对: %d", len(msg.Files))
	}
	// 实测口径：image_edit **不带 size**（顶层与 meta 都不带）。
	if body.Size != "" {
		t.Fatalf("image_edit 不该带顶层 size: %q", body.Size)
	}
	meta := msg.Extra["meta"].(map[string]any)
	if _, has := meta["size"]; has {
		t.Fatal("image_edit 不该带 extra.meta.size")
	}
	if _, has := meta["model"]; has {
		t.Fatal("image_edit 不该带 extra.meta.model（那是 t2i 的）")
	}
}

func TestBuildSubmitTextHasEmptyFilesArray(t *testing.T) {
	body := BuildSubmit("chat-1", "", "m", "hi", "auto", mustTime())
	if body.Messages[0].Files == nil {
		t.Fatal("纯文本的 files 必须是 []，不能是 null")
	}
	if len(body.Messages[0].Files) != 0 {
		t.Fatal("纯文本不该带附件")
	}
}
