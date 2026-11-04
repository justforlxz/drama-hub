package hongguo

// MKACG-683 AC-6：/player 的每一条出站请求日志都要带 caller=play|sync，
// 包括重试成功行与「重试次数用尽」行；启动期探测（无标记）不追加。

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/justforlxz/drama-hub/internal/plugin"
)

func TestFetchLogsCaller(t *testing.T) {
	body := readFixture(t, "page-series.html")

	t.Run("play 成功行", func(t *testing.T) {
		_, srv := newFakeSite(t, http.StatusOK, body)
		logs := &bytes.Buffer{}
		p := newLoggingPlugin(t, srv, logs)

		ctx := plugin.WithCaller(context.Background(), plugin.CallerPlay)
		if _, err := p.Work(ctx, testRef()); err != nil {
			t.Fatalf("Work: %v", err)
		}
		if !strings.Contains(logs.String(), "caller=play") {
			t.Errorf("日志缺 caller=play：%s", logs.String())
		}
	})

	t.Run("sync 重试成功行", func(t *testing.T) {
		fake, srv := newFakeSite(t, http.StatusOK, body)
		fake.setReplies([]route{
			{http.StatusOK, ""},
			{http.StatusOK, body},
		})
		logs := &bytes.Buffer{}
		p := newLoggingPlugin(t, srv, logs)

		ctx := plugin.WithCaller(context.Background(), plugin.CallerSync)
		if _, err := p.Work(ctx, testRef()); err != nil {
			t.Fatalf("Work: %v", err)
		}
		got := logs.String()
		for _, want := range []string{"上游请求重试成功", "caller=sync"} {
			if !strings.Contains(got, want) {
				t.Errorf("日志缺 %q：%s", want, got)
			}
		}
	})

	t.Run("sync 重试次数用尽行", func(t *testing.T) {
		_, srv := newFakeSite(t, http.StatusOK, "")
		logs := &bytes.Buffer{}
		p := newLoggingPlugin(t, srv, logs)

		ctx := plugin.WithCaller(context.Background(), plugin.CallerSync)
		if _, err := p.Work(ctx, testRef()); err == nil {
			t.Fatal("空体重试三次后仍成功，期望 ErrUpstream")
		}
		got := logs.String()
		for _, want := range []string{"上游请求重试次数用尽", "caller=sync"} {
			if !strings.Contains(got, want) {
				t.Errorf("日志缺 %q：%s", want, got)
			}
		}
	})

	t.Run("启动期探测不追加 caller", func(t *testing.T) {
		_, srv := newFakeSite(t, http.StatusOK, body)
		logs := &bytes.Buffer{}
		p := newLoggingPlugin(t, srv, logs)

		if _, err := p.Work(context.Background(), testRef()); err != nil {
			t.Fatalf("Work: %v", err)
		}
		if strings.Contains(logs.String(), "caller=") {
			t.Errorf("无标记的探测日志带了 caller：%s", logs.String())
		}
	})
}
