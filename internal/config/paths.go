package config

// 数据文件的路径推导。
//
// 部署里只有 state_file 是显式配置的，其余托管文件都跟它同目录 ——
// 一是保证落在同一个可写挂载卷里（data/ 是独立卷），
// 二是运维只需要记住一个路径。

import (
	"path/filepath"
	"strings"
)

// VideosFileName 视频任务映射的落盘文件名。
const VideosFileName = "videos.json"

// VideosFile 由 state_file 推导视频任务记录路径（空 state_file ⇒ 空路径 = 不落盘）。
//
// 🔴 为什么必须落盘：上游任务 ID 活 7 天、产物 24h，一个视频要 10~15 分钟才出片。
// 只存内存会导致重启后客户端手里的 id 直接 404，只能重新提交 —— 第二次计费。
func VideosFile(stateFile string) string {
	stateFile = strings.TrimSpace(stateFile)
	if stateFile == "" {
		return ""
	}
	dir := strings.TrimSpace(filepath.Dir(stateFile))
	if dir == "" || dir == "." {
		dir = "."
	}
	return filepath.Join(dir, VideosFileName)
}
