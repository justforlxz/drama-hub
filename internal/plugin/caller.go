package plugin

import "context"

// 调用来源标记（MKACG-683）：/player 的出站日志要能区分「同步在取数」与
// 「用户在播放」，否则同一行日志看不出是谁在打上游。
//
// 不改 Plugin / Resolver 的接口签名（MUST）：来源挂在 context 上，由框架的两个入口
// （stream.Resolver 与 sync.Syncer）各打一次标，插件实现只读。
const (
	// CallerPlay 是播放面解析直链。
	CallerPlay = "play"
	// CallerSync 是同步框架取剧集数据。
	CallerSync = "sync"
)

// callerKey 是 context 里存来源的类型化键。
type callerKey struct{}

// WithCaller 给 ctx 打上调用来源。空 caller 原样返回，不制造空标记。
func WithCaller(ctx context.Context, caller string) context.Context {
	if caller == "" {
		return ctx
	}
	return context.WithValue(ctx, callerKey{}, caller)
}

// CallerOf 取 ctx 上的调用来源；没有标记时返回空串。
func CallerOf(ctx context.Context) string {
	caller, _ := ctx.Value(callerKey{}).(string)
	return caller
}
