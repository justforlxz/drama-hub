// Package scheduler 是进程内单例刷新调度器。
//
// 契约唯一真值 docs/SCHEDULER-MODEL.md：按源跑「轮」，一轮 = 增量枚举 + 新剧灌库 +
// 存量轮转一批；间隔、批量、超时全部从 settings 现读，改完下一轮即按新值算，不重启。
//
// 本包只依赖 internal/plugin、internal/store 与一个注入的 Syncer；不认识 HTTP
// （手动触发与状态页由 internal/web 调 Trigger / Status），也不 import internal/sync
// （写库编排由装配根注入，接口定义在本包，见 DEV-STANDARDS「接口定义在消费方」）。
package scheduler
