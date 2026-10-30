package server

import "testing"

func TestRotateWindow(t *testing.T) {
	names := []string{"a", "b", "c", "d", "e"}

	got, next := rotateWindow(names, 0, 2)
	if len(got) != 2 || got[0] != "a" || got[1] != "b" || next != 2 {
		t.Fatalf("第一轮错: %v next=%d", got, next)
	}
	got, next = rotateWindow(names, next, 2)
	if got[0] != "c" || got[1] != "d" || next != 4 {
		t.Fatalf("第二轮错: %v next=%d", got, next)
	}
	// 越界环绕：这是"后面的账号永远轮不到"那个 bug 的判据。
	got, next = rotateWindow(names, next, 2)
	if got[0] != "e" || got[1] != "a" || next != 1 {
		t.Fatalf("环绕错: %v next=%d", got, next)
	}
	got, next = rotateWindow(names, 3, 99)
	if len(got) != 5 || got[0] != "a" || next != 0 {
		t.Fatalf("全取错: %v next=%d", got, next)
	}
	if got, _ := rotateWindow(nil, 0, 4); got != nil {
		t.Fatalf("空列表应返回 nil: %v", got)
	}
	if got, _ := rotateWindow(names, 2, 0); got != nil {
		t.Fatalf("n=0 应返回 nil: %v", got)
	}
}
