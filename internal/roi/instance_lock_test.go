package roi

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstanceLockRejectsSecondAppAndSymlink(t *testing.T) {
	c := testConfig(t)
	a, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if second, err := New(c); err == nil {
		second.Close()
		t.Fatal("second scheduler accepted")
	}
	alias := filepath.Join(t.TempDir(), "alias.db")
	if err := os.Symlink(c.Database, alias); err != nil {
		t.Fatal(err)
	}
	other := c
	other.Database = alias
	if second, err := New(other); err == nil {
		second.Close()
		t.Fatal("symlink bypassed instance lock")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := New(c)
	if err != nil {
		t.Fatal("lock retained after Close", err)
	}
	second.Close()
	if _, err := os.Stat(c.Database + ".roi.lock"); err != nil {
		t.Fatal(err)
	}
}
func TestInstanceLockReleasedOnProcessExit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "roi.db")
	cmd := exec.Command(os.Args[0], "-test.run=^TestInstanceLockProcessHelper$")
	cmd.Env = append(os.Environ(), "ROI_LOCK_TEST_PATH="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "locked" {
		t.Fatal(line, err)
	}
	if lock, err := lockInstance(path); err == nil {
		lock.Close()
		t.Fatal("cross-process lock failed")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	lock, err := lockInstance(path)
	if err != nil {
		t.Fatal("crash retained lock", err)
	}
	lock.Close()
}
func TestInstanceLockProcessHelper(t *testing.T) {
	path := os.Getenv("ROI_LOCK_TEST_PATH")
	if path == "" {
		return
	}
	lock, err := lockInstance(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	_, _ = os.Stdout.WriteString("locked\n")
	select {}
}
