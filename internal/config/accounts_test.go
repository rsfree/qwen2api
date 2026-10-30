package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAccountsFileFollowsStateFile(t *testing.T) {
	// 部署里 state_file=/app/data/state.json ⇒ accounts.json 必须同目录（可写挂载点）。
	if got := AccountsFile("/app/data/state.json"); got != "/app/data/accounts.json" {
		t.Fatalf("路径推导不对: %s", got)
	}
	if got := AccountsFile(""); got != AccountsFileName {
		t.Fatalf("空 state_file 应退回当前目录: %s", got)
	}
	if got := AccountsFile("state.json"); got != AccountsFileName {
		t.Fatalf("同目录应只给文件名: %s", got)
	}
}

func TestLoadAccountsMissingMeansNotManaged(t *testing.T) {
	// 文件不存在 ⇒ (nil, nil)：用 config.json 的账号。
	// 区分 nil 与空切片是关键 —— 空切片表示"用户把号全删了"，不能再回退。
	accs, err := LoadAccountsOverride(filepath.Join(t.TempDir(), "nope.json"))
	if accs != nil || err != nil {
		t.Fatalf("缺文件应返回 (nil,nil)，得到 (%v,%v)", accs, err)
	}
}

func TestLoadAccountsEmptyTakesOver(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, AccountsFileName)
	if err := os.WriteFile(path, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	accs, err := LoadAccountsOverride(path)
	if err != nil {
		t.Fatal(err)
	}
	if accs == nil {
		t.Fatal("空数组必须返回非 nil（表示接管且列表为空）")
	}
	if len(accs) != 0 {
		t.Fatalf("应为空，得到 %d", len(accs))
	}
}

func TestLoadAccountsCorruptIsError(t *testing.T) {
	// 坏文件必须报错，不能静默退回 config —— 否则面板里删掉的号重启就复活了。
	dir := t.TempDir()
	path := filepath.Join(dir, AccountsFileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadAccountsOverride(path); err == nil {
		t.Fatal("坏 JSON 应报错")
	}
}

func TestSaveAccountsRoundTripAndPerm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", AccountsFileName)
	in := []Account{
		{Name: "a", Email: "a@x.com", Password: "pw", Note: "备注"},
		{Name: "b", Email: "b@x.com", Password: "pw2"},
	}
	if err := SaveAccounts(path, in); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// 里面是密码 ⇒ 必须 0600
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("权限应为 0600，得到 %o", mode)
	}
	out, err := LoadAccountsOverride(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].Note != "备注" || out[1].Password != "pw2" {
		t.Fatalf("往返不一致: %+v", out)
	}
}

func TestSaveAccountsAtomicLeavesNoTemp(t *testing.T) {
	// 原子写用同目录临时文件；成功后必须没有残留（否则目录越跑越脏）。
	dir := t.TempDir()
	path := filepath.Join(dir, AccountsFileName)
	if err := SaveAccounts(path, []Account{{Name: "a", Email: "a@x.com"}}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != AccountsFileName {
			t.Fatalf("残留临时文件: %s", e.Name())
		}
	}
}
