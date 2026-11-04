package stream

import (
	"errors"
	"fmt"
	"testing"

	"github.com/justforlxz/drama-hub/internal/plugin"
)

// TestFailureReason 钉住 episode_stream_fail.reason 的映射：分类只在这一处，
// 预取器不认识插件哨兵（ADR-0017）。
func TestFailureReason(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"rejected", fmt.Errorf("剧集 1: %w", plugin.ErrRejected), FailureRejected},
		{"upstream", fmt.Errorf("剧集 1: %w", plugin.ErrUpstream), FailureUpstream},
		{"schema", plugin.ErrSchema, FailureSchema},
		{"no_stream", plugin.ErrNoStream, FailureNoStream},
		{"login", plugin.ErrLoginRequired, FailureLogin},
		{"unsupported", plugin.ErrUnsupported, FailureUnsupported},
		// ErrNotFound 不在固定取值表里：归 other（记录不丢，只丢精度）。
		{"not_found", plugin.ErrNotFound, FailureOther},
		{"plain", errors.New("库连接断了"), FailureOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := FailureReason(tc.err); got != tc.want {
				t.Errorf("FailureReason(%v) = %q, 期望 %q", tc.err, got, tc.want)
			}
		})
	}
}
