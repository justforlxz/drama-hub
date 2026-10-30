package cache

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// helperBodyBytes 是子进程每一轮写入的字节数：足够大，父进程才抓得到「写到一半」
// 的窗口（64 MB 在 tmpfs 上也要几十毫秒，远大于 500 µs 的轮询间隔）。
const helperBodyBytes = 64 << 20

// AC：写入中途 kill，缓存目录无半截文件。
//
// 做法是真的 kill：拉起一个子进程用生产代码的 writeFileAtomic 循环写 64 MB，父进程
// 在 tmp 里出现「写到一半」的临时文件时 SIGKILL 它，然后检查：
//   - 三个内容目录里只有完整文件（长度 == 64 MB），没有半截；
//   - 整个过程中目标路径只可能是「不存在」或「完整文件」——这就是「读到半截文件的
//     可能为零」的直接断言；
//   - 被打断的半截文件落在 tmp/ 里，由淘汰的 tmp 清扫收掉。
func TestWriteKilledMidwayLeavesNoPartialFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	tmpDir := filepath.Join(root, dirTmp)
	target := filepath.Join(root, dirSegment, "ab", "ab-killed.ts")

	cmd := exec.Command(os.Args[0], "-test.run=^TestWriteHelperProcess$", "-test.timeout=300s")
	cmd.Env = append(os.Environ(),
		"CACHE_KILL_HELPER_TARGET="+target,
		"CACHE_KILL_HELPER_TMP="+tmpDir,
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("拉起子进程: %v", err)
	}
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	// 等到「tmp 里有一个 0 < 长度 < 64 MB 的临时文件」，即写入进行到一半。
	deadline := time.Now().Add(60 * time.Second)
	killed := false
	for time.Now().Before(deadline) {
		if hasPartialTemp(t, tmpDir, helperBodyBytes) {
			if err := cmd.Process.Kill(); err != nil {
				t.Fatalf("kill 子进程: %v", err)
			}
			killed = true
			break
		}
		// 途中严格断言：目标路径要么不存在，要么是完整文件（永不半截）。
		assertTargetCompleteOrAbsent(t, target, helperBodyBytes)
		time.Sleep(500 * time.Microsecond)
	}
	_ = cmd.Wait() // SIGKILL 的退出错误是预期的
	waited = true
	if !killed {
		t.Fatal("子进程没有进入「写到一半」的状态，无法验证中途 kill")
	}

	// 内容目录里只能是完整文件。
	files := contentFiles(t, root)
	for _, rel := range files {
		info, err := os.Stat(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("stat %s: %v", rel, err)
		}
		if info.Size() != helperBodyBytes {
			t.Fatalf("内容目录里出现半截文件 %s：%d 字节，期望 %d", rel, info.Size(), helperBodyBytes)
		}
	}
	assertTargetCompleteOrAbsent(t, target, helperBodyBytes)
	for _, line := range findTree(t, root) {
		t.Logf("find %s/%s", root, line)
	}

	// 被打断的半截文件在 tmp/ 里（最多一个：每轮 rename 成功后同名覆盖）。
	if got := len(tempFiles(t, tmpDir)); got > 1 {
		t.Fatalf("tmp 里留下 %d 个临时文件，期望至多 1 个", got)
	}
	if _, err := sweepTmp(tmpDir, time.Now().Add(2*tmpMaxAge)); err != nil {
		t.Fatalf("清扫 tmp: %v", err)
	}
	if got := tempFiles(t, tmpDir); len(got) != 0 {
		t.Fatalf("清扫后 tmp 仍有文件: %v", got)
	}
}

// TestWriteHelperProcess 是被 TestWriteKilledMidwayLeavesNoPartialFile 拉起的子进程：
// 无限循环地通过生产代码的 writeFileAtomic 写同一个目标路径，父进程在任意时刻 kill
// 它。没有 CACHE_KILL_HELPER_TARGET 时直接跳过（正常 `go test ./...` 不跑）。
func TestWriteHelperProcess(t *testing.T) {
	target := os.Getenv("CACHE_KILL_HELPER_TARGET")
	if target == "" {
		t.Skip("只作为 TestWriteKilledMidwayLeavesNoPartialFile 的子进程运行")
	}
	tmpDir := os.Getenv("CACHE_KILL_HELPER_TMP")
	body := bytes.Repeat([]byte("x"), helperBodyBytes)
	for {
		if err := writeFileAtomic(tmpDir, target, body); err != nil {
			t.Fatalf("writeFileAtomic: %v", err)
		}
	}
}

// hasPartialTemp 判断 dir 里是否有 0 < 长度 < fullSize 的普通文件（写到一半的临时文件）。
func hasPartialTemp(t *testing.T, dir string, fullSize int64) bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false // 目录还没建出来
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if size := info.Size(); size > 0 && size < fullSize {
			return true
		}
	}
	return false
}

// assertTargetCompleteOrAbsent 断言目标路径要么不存在、要么正好是完整长度。
func assertTargetCompleteOrAbsent(t *testing.T, target string, fullSize int64) {
	t.Helper()
	info, err := os.Stat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatalf("stat %s: %v", target, err)
	}
	if info.Size() != fullSize {
		t.Fatalf("目标文件出现了半截内容 %s：%d 字节，期望 0（不存在）或 %d", target, info.Size(), fullSize)
	}
}

// tempFiles 返回 tmp 目录里的普通文件名（目录不存在视为空）。
func tempFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("列 tmp 目录: %v", err)
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		out = append(out, entry.Name())
	}
	return out
}
