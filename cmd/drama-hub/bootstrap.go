package main

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/justforlxz/drama-hub/internal/store"
)

// envBootstrap 是「空卷首次启动」的配置入口：env → settings 键。
//
// 只放「没有它就起不来」的键：后台配置接口（PATCH /api/settings）要等服务起来才能调，
// 而空卷上出流签名密钥（缺了 /video 恒 500）与镜像里的缓存目录是启动前就必须有的值；
// cache_dir 在镜像里也必须由 env 指定，否则相对路径会落成 /data/data/cache（WORKDIR /data）。
//
// WebDAV 账号密码**不走 env**（lxz 2026-10-31 定）：它们是后台配置项，由 MKACG-622 的
// 配置写入接口管理；env 再种一份就会出现两个真值。
var envBootstrap = []struct {
	env string
	key store.SettingKey
}{
	{"DRAMA_HUB_STREAM_SECRET", store.KeyStreamSecret},
	{"DRAMA_HUB_CACHE_DIR", store.KeyCacheDir},
}

// bootstrapSettings 把 env 引导值写进 settings，只在键还没有行时写一次。
//
// 幂等：已有值一律不覆盖，所以重启不会把手改过的 settings 覆盖回 env。
// env 缺省 → 跳过（不写空值，不让服务起不来）；值非法（secret 不足 32 字节、
// webdav 用户/密码为空等）→ 记一条 warn 并跳过，不写坏值、不 panic：看板上
// 「未设置」就是这条原因。写库失败同样只 warn，不挡启动。
func bootstrapSettings(ctx context.Context, settings *store.Settings, logger *slog.Logger) {
	for _, item := range envBootstrap {
		raw := os.Getenv(item.env)
		if raw == "" {
			continue
		}
		written, err := settings.SetIfAbsent(ctx, item.key, raw)
		if err != nil {
			// 只报 env 名与键名：值是凭据，不进日志。
			logger.Warn("env 引导配置被拒绝，未写入", "env", item.env, "key", string(item.key), "err", err)
			continue
		}
		if written {
			logger.Info("env 引导配置已写入", "env", item.env, "key", string(item.key))
		}
	}
}

// isKnownEnv 判断 env 名是不是启动期真正会读的变量：main 的三项 + 管理令牌
// （DRAMA_HUB_ADMIN_TOKEN）+ envBootstrap 的每次启动引导项。
func isKnownEnv(name string) bool {
	switch name {
	case "DRAMA_HUB_DB", "DRAMA_HUB_ADDR", "DRAMA_HUB_LOG_LEVEL", "DRAMA_HUB_ADMIN_TOKEN":
		return true
	}
	for _, item := range envBootstrap {
		if item.env == name {
			return true
		}
	}
	return false
}

// warnUnknownEnv 把拼错/已废弃的 `DRAMA_HUB_*` 变量打成一条 warn。
//
// env 名拼错时进程只静默忽略——那正是「配了却没生效」的一半来源（MKACG-617 的场景
// 审查记录）。只报变量名，不报值（可能是凭据）。
func warnUnknownEnv(logger *slog.Logger) {
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "DRAMA_HUB_") && !isKnownEnv(name) {
			logger.Warn("未知的 DRAMA_HUB_* 环境变量被忽略（拼错了？）",
				"env", name, "supported", supportedEnv())
		}
	}
}

// supportedEnv 返回启动期支持的 env 名，拼错时一起打出来（B4：让人一眼看见该用哪个）。
//
// WebDAV 账号密码不在此列：它们由后台配置接口管理（MKACG-622），不是 env。
func supportedEnv() []string {
	out := []string{"DRAMA_HUB_DB", "DRAMA_HUB_ADDR", "DRAMA_HUB_LOG_LEVEL", "DRAMA_HUB_ADMIN_TOKEN"}
	for _, item := range envBootstrap {
		out = append(out, item.env)
	}
	return out
}
