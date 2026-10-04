package frequency

import (
	"path/filepath"
	"testing"
	"time"
)

func TestRollingWindow(t *testing.T) {
	dir := t.TempDir()
	c, err := NewCap(time.Minute, 2, filepath.Join(dir, "freq.json"))
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	for i := 0; i < 2; i++ {
		ok, err := c.Record("u1", t0.Add(time.Duration(i)*time.Second))
		if err != nil || !ok {
			t.Fatalf("record %d should pass: ok=%v err=%v", i, ok, err)
		}
	}
	if c.Allowed("u1", t0.Add(30*time.Second)) {
		t.Fatal("cap reached: third impression within window must be blocked")
	}
	if ok, _ := c.Record("u1", t0.Add(30*time.Second)); ok {
		t.Fatal("record at cap must fail and not persist")
	}
	if n := c.Count("u1", t0.Add(30*time.Second)); n != 2 {
		t.Fatalf("want count 2, got %d", n)
	}
	// 窗口滚动：61s 后最早记录滑出，允许一次新曝光。
	t1 := t0.Add(61 * time.Second)
	if !c.Allowed("u1", t1) {
		t.Fatal("window should have rolled")
	}
	if ok, _ := c.Record("u1", t1); !ok {
		t.Fatal("record after window roll should pass")
	}
	// 不同用户互不影响。
	if !c.Allowed("u2", t1) {
		t.Fatal("other user must be unaffected")
	}
}

func TestPersistenceAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "freq.json")
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	c1, err := NewCap(time.Minute, 2, path)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := c1.Record("u1", t0); !ok {
		t.Fatal("record failed")
	}
	if ok, _ := c1.Record("u1", t0.Add(time.Second)); !ok {
		t.Fatal("record failed")
	}

	// 模拟进程重启：新建频控器，从磁盘恢复。
	c2, err := NewCap(time.Minute, 2, path)
	if err != nil {
		t.Fatal(err)
	}
	if n := c2.Count("u1", t0.Add(2*time.Second)); n != 2 {
		t.Fatalf("after restart want count 2, got %d", n)
	}
	if c2.Allowed("u1", t0.Add(2*time.Second)) {
		t.Fatal("after restart cap must still be enforced")
	}
	// 滑窗后恢复容量。
	if !c2.Allowed("u1", t0.Add(61*time.Second)) {
		t.Fatal("after restart window must still roll")
	}
}

func TestRejectDoesNotPersist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "freq.json")
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	c1, _ := NewCap(time.Minute, 1, path)
	_, _ = c1.Record("u1", t0)
	if ok, _ := c1.Record("u1", t0.Add(time.Second)); ok {
		t.Fatal("second record must be rejected")
	}
	c2, _ := NewCap(time.Minute, 1, path)
	if n := c2.Count("u1", t0.Add(time.Second)); n != 1 {
		t.Fatalf("rejected record must not be persisted, count=%d", n)
	}
}
