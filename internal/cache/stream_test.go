package cache

// MKACG-686：边生成边出流的底层契约。
//
//   - 读者能在生成结束前读到已经落盘的字节（首片不等整集）；
//   - 半成品（tmp/.partial-*）永远不会被当成完整产物服务——即使它的字节
//     恰好与成品一样（进程重启后只剩这些残留，验收第 6 条）。

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestStreamReaderSeesBytesBeforeGenerationEnds 钉住「边生成边读」：
// 生成写出一段就阻塞，读者仍必须立刻拿到这一段。
func TestStreamReaderSeesBytesBeforeGenerationEnds(t *testing.T) {
	tc := newTestCache(t)
	ctx := context.Background()
	spec := segSpec(testUID)

	head := "\x00\x00\x00\x18ftypiso5"
	started := make(chan struct{})
	release := make(chan struct{})
	s, err := tc.c.GetOrStream(ctx, spec, func(_ context.Context, w io.Writer) error {
		if _, err := io.WriteString(w, head); err != nil {
			return err
		}
		close(started)
		<-release
		_, err := io.WriteString(w, "payload")
		return err
	}, false)
	if err != nil {
		t.Fatalf("GetOrStream: %v", err)
	}
	<-started

	rd, err := s.Reader(ctx, 0)
	if err != nil {
		t.Fatalf("Reader: %v", err)
	}
	buf := make([]byte, len(head))
	if _, err := io.ReadFull(rd, buf); err != nil {
		t.Fatalf("读首块（生成还没结束）: %v", err)
	}
	if string(buf) != head {
		t.Fatalf("首块 = %q, 期望 %q", buf, head)
	}
	close(release)
	rest, err := io.ReadAll(rd)
	if err != nil {
		t.Fatalf("读完: %v", err)
	}
	rd.Close()
	if got := head + string(rest); got != head+"payload" {
		t.Fatalf("完整内容 = %q", got)
	}
	if err := s.Wait(ctx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	// 生成结束后就是普通缓存命中：第二次不再调用生成函数。
	gens := 0
	if _, err := tc.c.GetOrGenerate(ctx, spec, func(context.Context) ([]byte, error) {
		gens++
		return nil, io.ErrUnexpectedEOF
	}); err != nil {
		t.Fatalf("第二次取: %v", err)
	}
	if gens != 0 {
		t.Fatalf("命中缓存又调了 %d 次生成", gens)
	}
}

// TestGetOrGenerateRetriesAfterLeaderCancel：领跑者被取消（后台预热被 preempt、换集
// 取消旧任务）时，挂在同一份在途生成上的读者不能把 context.Canceled 当内容失败 ——
// 换一个新领跑者重试一次，仍然拿到完整字节（MKACG-686 代码审查 blocker）。
func TestGetOrGenerateRetriesAfterLeaderCancel(t *testing.T) {
	tc := newTestCache(t)
	spec := segSpec(testUID)

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	defer cancelLeader()

	started := make(chan struct{})
	leader, err := tc.c.GetOrStream(leaderCtx, spec, func(ctx context.Context, _ io.Writer) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}, true)
	if err != nil {
		t.Fatalf("GetOrStream（领跑者）: %v", err)
	}
	<-started

	var gens atomic.Int32
	type result struct {
		body []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		body, err := tc.c.GetOrGenerate(context.Background(), spec, func(context.Context) ([]byte, error) {
			gens.Add(1)
			return []byte(testSegmentBody), nil
		})
		done <- result{body: body, err: err}
	}()

	// 等读者真的挂到这次生成上：played 是 Reader 在锁内打的标。
	for i := 0; i < 200; i++ {
		leader.mu.Lock()
		joined := leader.played
		leader.mu.Unlock()
		if joined {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	cancelLeader()

	res := <-done
	if res.err != nil {
		t.Fatalf("领跑者被取消后读者拿到错误: %v", res.err)
	}
	if string(res.body) != testSegmentBody {
		t.Fatalf("读者拿到的字节 = %q", res.body)
	}
	if got := gens.Load(); got != 1 {
		t.Fatalf("重领后生成次数 = %d, 期望 1", got)
	}
}

// TestPartialFileNeverServedAsComplete：tmp/.partial-* 是半成品，不进 artifact
// 表。即使残留的字节与成品相同，也必须重新生成，而不是把残留当成品发出去。
func TestPartialFileNeverServedAsComplete(t *testing.T) {
	tc := newTestCache(t)
	ctx := context.Background()
	spec := segSpec(testUID)

	tmp := layoutOf(tc.root).tmpDir()
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatalf("建 tmp: %v", err)
	}
	partial := filepath.Join(tmp, ".partial-1234")
	if err := os.WriteFile(partial, []byte(testSegmentBody), 0o644); err != nil {
		t.Fatalf("写半成品: %v", err)
	}

	if ok, err := tc.c.Has(ctx, spec); err != nil || ok {
		t.Fatalf("Has = %v, err = %v；半成品不该算命中", ok, err)
	}

	gens := 0
	body, err := tc.c.GetOrGenerate(ctx, spec, func(context.Context) ([]byte, error) {
		gens++
		return []byte(testSegmentBody), nil
	})
	if err != nil {
		t.Fatalf("GetOrGenerate: %v", err)
	}
	if gens != 1 {
		t.Fatalf("生成调用 %d 次, 期望 1（半成品不参与单飞）", gens)
	}
	if string(body) != testSegmentBody {
		t.Fatalf("内容 = %q", body)
	}
	// 正规产物必须在 seg/ 下并登记；残留只等 tmp 清扫规则处理。
	row, ok, err := tc.c.cached(ctx, spec)
	if err != nil || !ok {
		t.Fatalf("cached = %v, err = %v", ok, err)
	}
	if filepath.Dir(filepath.Dir(row.Path)) != filepath.Join(tc.root, dirSegment) {
		t.Fatalf("产物路径 = %q, 期望在 %s 下", row.Path, dirSegment)
	}
	if _, err := os.Stat(partial); err != nil {
		t.Fatalf("半成品不该在生成时被当成输入删掉: %v", err)
	}
}
