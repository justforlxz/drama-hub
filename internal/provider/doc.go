// Package provider 封装短剧数据源适配（官网 SSR 主力 / App 接口回退）。
//
// 依赖方向：rank 30，可 import rank 20/10 的 internal 包（cache、store）；
// rank 40/50/60 的包可 import 本包；与同为 30 的 vfs 同级、互不 import。
// 规则见 docs/DEV-STANDARDS.md「代码组织」，由 scripts/check-deps.sh 强制。
//
// 边界：本阶段提供官网 SSR 数据源的元数据同步（SyncSeries）与按需直链解析
// （FetchDirectURL）。App 接口回退不在这里。官网列表页的 seriesDetail.vid_list
// 只给出集标识，单集页 SSR 的 contentUrl 是约 10 秒预览片而非整集，因此同步不写
// episode.play_url，整集字节来源由上层决定（见 docs/DEV-STANDARDS.md「数据源」）。
package provider
