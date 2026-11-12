package config

// Storage 开关语义的单测。
//
// 这里锁的是三条产品纪律：
//   1. 默认开（没配置也要开）—— "没处理完不发 URL" 是默认行为；
//   2. 显式 false 能关（"没配" 与 "主动关" 必须可区分）；
//   3. 凭据不全时降级为关，而不是让每条视频都失败。

import "testing"

func boolPtr(b bool) *bool { return &b }

func TestStorageEnabledByDefault(t *testing.T) {
	// 零值（没配 enabled，但凭据齐全）应当视为开启。
	s := Storage{Endpoint: "https://oss.example", Bucket: "cdn",
		AccessKey: "ak", SecretKey: "sk"}
	if !s.IsEnabled() {
		t.Fatal("未配置 enabled 时应默认开启")
	}
}

func TestStorageExplicitOff(t *testing.T) {
	s := Storage{Enabled: boolPtr(false), Endpoint: "https://oss.example",
		Bucket: "cdn", AccessKey: "ak", SecretKey: "sk"}
	if s.IsEnabled() {
		t.Fatal("显式 false 必须能关掉")
	}
}

func TestStorageMissingCredentialsDowngrades(t *testing.T) {
	// 默认开但缺凭据 ⇒ 降级为关。宁可透传（带水印），也不能让每条视频都失败。
	for _, miss := range []Storage{
		{Bucket: "cdn", AccessKey: "ak", SecretKey: "sk"},                   // 缺 endpoint
		{Endpoint: "https://oss.example", AccessKey: "ak", SecretKey: "sk"}, // 缺 bucket
		{Endpoint: "https://oss.example", Bucket: "cdn", SecretKey: "sk"},   // 缺 ak
		{Endpoint: "https://oss.example", Bucket: "cdn", AccessKey: "ak"},   // 缺 sk
	} {
		if miss.IsEnabled() {
			t.Fatalf("缺凭据时必须降级为关: %+v", miss)
		}
		if len(miss.MissingKeys()) == 0 {
			t.Fatalf("应报出缺失项: %+v", miss)
		}
	}
}
