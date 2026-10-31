package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// settings 表的哨兵错误。调用方用 errors.Is 判断，不比较错误字符串。
var (
	// ErrUnknownSetting 表示 key 不在注册表里。读写都会返回它。
	ErrUnknownSetting = errors.New("未知的配置项")
	// ErrInvalidSetting 表示值未通过校验（解析失败、越界、类型不符），此时不落库。
	ErrInvalidSetting = errors.New("非法的配置值")
	// ErrSettingMissing 表示该项在库中不存在：必填项从未配置，或可选覆盖项按
	// 「未设置」（继承全局）处理。用 OptionalInt 读可选覆盖项可避开这个错误。
	ErrSettingMissing = errors.New("必填配置项缺失")
)

// SettingKey 是 settings 表的键。
//
// 注册表认两组 key：下面列出的固定 key，以及插件配置的前缀模式
// plugin.<源>.<项>（真值见 docs/DEV-STANDARDS.md「配置」表与
// docs/PLUGIN-CONTRACT.md「配置」）。
type SettingKey string

// settings 表的 key 常量。
const (
	KeyStrmPrefix   SettingKey = "strm_prefix"
	KeyStreamSecret SettingKey = "stream_secret"
	KeySegTargetSec SettingKey = "seg_target_sec"
	KeyWebDAVUser   SettingKey = "webdav_user"
	KeyWebDAVPass   SettingKey = "webdav_pass"
	KeyCacheDir     SettingKey = "cache_dir"
	KeyCacheMaxMB   SettingKey = "cache_max_mb"
	KeyCacheLRUDays SettingKey = "cache_lru_days"
	// 刷新调度的三项（范围与语义见 docs/SCHEDULER-MODEL.md「配置」）。
	KeyRefreshIntervalSec SettingKey = "refresh_interval_sec"
	KeyRefreshBatch       SettingKey = "refresh_batch"
	KeyRefreshTimeoutSec  SettingKey = "refresh_timeout_sec"
)

// 插件配置的项名（docs/PLUGIN-CONTRACT.md「配置」）。
const (
	PluginItemEnabled        = "enabled"
	PluginItemBaseURL        = "base_url"
	PluginItemRequestTimeout = "request_timeout_sec"
	// PluginItemRefreshInterval 是源级刷新间隔覆盖。库中无行 = 继承全局
	// refresh_interval_sec，所以它必须用 Settings.OptionalInt 读（Int 读到「未设置」
	// 返回 ErrSettingMissing，避免把「继承」当成某个具体数字）。
	PluginItemRefreshInterval = "refresh_interval_sec"
)

// PluginSettingKey 拼出插件配置键 plugin.<源>.<项>。
//
// 键的格式与校验都在本包：specFor 只认上列四项（enabled / base_url /
// request_timeout_sec / refresh_interval_sec），非法源名或未知项返回
// ErrUnknownSetting。调用方（装配根、状态页）不自己拼这个字符串。
func PluginSettingKey(source, item string) SettingKey {
	return SettingKey("plugin." + source + "." + item)
}

// minSecretBytes 是出流签名密钥的最小字节数（ADR-0003 的 HMAC 密钥）。
const minSecretBytes = 32

// settingsDDL 是 settings 表的唯一 DDL。
//
// F3 的 migrations/0001_init.sql 必须写入同一份 DDL（保留 IF NOT EXISTS），
// 两处幂等共存；改一处必须同步改另一处。
const settingsDDL = `
CREATE TABLE IF NOT EXISTS settings (
	key        TEXT PRIMARY KEY,
	value_json TEXT NOT NULL,
	updated_at TEXT NOT NULL
)`

// EnsureSettings 建 settings 表，可重复调用。
//
// 幂等：DDL 带 IF NOT EXISTS，重复执行不报错、不丢数据。
func EnsureSettings(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, settingsDDL); err != nil {
		return fmt.Errorf("建 settings 表: %w", err)
	}
	return nil
}

// Settings 是 settings 表的读写入口。
//
// 不缓存：每次读都查库，保证「改值后下一次读取立即生效」（AGENTS.md 硬约束 5）。
type Settings struct {
	db *sql.DB
}

// NewSettings 构造 Settings。
func NewSettings(db *sql.DB) *Settings {
	return &Settings{db: db}
}

// Set 校验并写入一个配置项。
//
// 校验失败返回 ErrInvalidSetting 且不写库；key 未注册返回 ErrUnknownSetting。
// 写成功时 updated_at 更新为当前 UTC RFC3339 时间。
func (s *Settings) Set(ctx context.Context, key SettingKey, raw string) error {
	value, err := parseSetting(key, raw)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("编码配置 %s: %w", key, err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO settings(key, value_json, updated_at) VALUES(?, ?, ?)
		 ON CONFLICT(key) DO UPDATE
		 SET value_json = excluded.value_json, updated_at = excluded.updated_at`,
		string(key), string(encoded), time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		return fmt.Errorf("写入配置 %s: %w", key, err)
	}
	return nil
}

// SetMany 在一个事务里校验并写入多个配置项：全部通过才落库。
//
// 与 Set 复用同一条校验路径（parseSetting），所以未知键 / 非法的值分别返回
// ErrUnknownSetting / ErrInvalidSetting，此时一行都不写 —— 管理面按「整批拒绝」处理，
// 避免半批生效后没人说得清零不零。返回归一化后的类型化值（string / int / bool），
// 调用方可以直接回给前端（`strm_prefix` 的尾 `/` 就是在这里去掉的）。
func (s *Settings) SetMany(ctx context.Context, values map[SettingKey]string) (map[SettingKey]any, error) {
	// 按 key 排序：同一批写入的报错顺序稳定（先报哪个非法项不随 map 迭代变化）。
	keys := make([]SettingKey, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	parsed := make(map[SettingKey]any, len(values))
	encoded := make(map[SettingKey]string, len(values))
	for _, key := range keys {
		value, err := parseSetting(key, values[key])
		if err != nil {
			return nil, err
		}
		buf, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("编码配置 %s: %w", key, err)
		}
		parsed[key] = value
		encoded[key] = string(buf)
	}
	if len(keys) == 0 {
		return parsed, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开始写配置事务: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// 同一批次共用一个 updated_at：这一批改的配置是同一时刻生效的。
	now := time.Now().UTC().Format(time.RFC3339)
	for _, key := range keys {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO settings(key, value_json, updated_at) VALUES(?, ?, ?)
			 ON CONFLICT(key) DO UPDATE
			 SET value_json = excluded.value_json, updated_at = excluded.updated_at`,
			string(key), encoded[key], now,
		); err != nil {
			return nil, fmt.Errorf("写入配置 %s: %w", key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交配置写入: %w", err)
	}
	return parsed, nil
}

// SetIfAbsent 校验并在 key 还没有行时写入，返回是否真的写了。
//
// 已有的值一律不动，因此可以每次启动都调用（幂等）。校验走 Set 的同一条路径：
// 非法值返回 ErrInvalidSetting 且不落库。用途是首次启动的 env 引导
// （cmd/drama-hub 的 DRAMA_HUB_* → settings）：空卷靠它种出必填项，重启不覆盖手改值。
func (s *Settings) SetIfAbsent(ctx context.Context, key SettingKey, raw string) (bool, error) {
	value, err := parseSetting(key, raw)
	if err != nil {
		return false, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return false, fmt.Errorf("编码配置 %s: %w", key, err)
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO settings(key, value_json, updated_at) VALUES(?, ?, ?)
		 ON CONFLICT(key) DO NOTHING`,
		string(key), string(encoded), time.Now().UTC().Format(time.RFC3339),
	)
	if err != nil {
		return false, fmt.Errorf("写入配置 %s: %w", key, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("统计配置 %s 的写入结果: %w", key, err)
	}
	return n == 1, nil
}

// String 读取一个字符串配置项。
//
// 库中无行时取默认值（如 strm_prefix 的 ""）；必填项无行返回 ErrSettingMissing；
// key 未注册返回 ErrUnknownSetting；整型 key 用本方法读返回 ErrInvalidSetting。
func (s *Settings) String(ctx context.Context, key SettingKey) (string, error) {
	value, err := s.value(ctx, key)
	if err != nil {
		return "", err
	}
	str, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%w: %s 是整型配置，用 Int 读取", ErrInvalidSetting, key)
	}
	return str, nil
}

// Int 读取一个整型配置项。
//
// 无行时取默认值；必填项无行返回 ErrSettingMissing；key 未注册返回 ErrUnknownSetting；
// 字符串 key 用本方法读返回 ErrInvalidSetting。
func (s *Settings) Int(ctx context.Context, key SettingKey) (int, error) {
	value, err := s.value(ctx, key)
	if err != nil {
		return 0, err
	}
	n, ok := value.(int)
	if !ok {
		return 0, fmt.Errorf("%w: %s 是字符串配置，用 String 读取", ErrInvalidSetting, key)
	}
	return n, nil
}

// Bool 读取一个布尔配置项。
//
// 无行时取默认值；必填项无行返回 ErrSettingMissing；key 未注册返回 ErrUnknownSetting；
// 非布尔 key 用本方法读返回 ErrInvalidSetting。
func (s *Settings) Bool(ctx context.Context, key SettingKey) (bool, error) {
	value, err := s.value(ctx, key)
	if err != nil {
		return false, err
	}
	b, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("%w: %s 不是布尔配置", ErrInvalidSetting, key)
	}
	return b, nil
}

// OptionalInt 读取一个可缺省的整型配置项。
//
// 与 Int 的区别只在「库中无该行」：found=false 且不报错，调用方按「未设置」处理
// （典型场景：plugin.<源>.refresh_interval_sec 空 = 继承全局 refresh_interval_sec）。
// key 未注册返回 ErrUnknownSetting，非整型 key 返回 ErrInvalidSetting。
func (s *Settings) OptionalInt(ctx context.Context, key SettingKey) (value int, found bool, err error) {
	spec, ok := specFor(key)
	if !ok {
		return 0, false, fmt.Errorf("%w: %q", ErrUnknownSetting, key)
	}
	if spec.kind != kindInt {
		return 0, false, fmt.Errorf("%w: %s 不是整型配置", ErrInvalidSetting, key)
	}
	encoded, found, err := s.load(ctx, key)
	if err != nil || !found {
		return 0, false, err
	}
	if err := json.Unmarshal([]byte(encoded), &value); err != nil {
		return 0, false, fmt.Errorf("解码配置 %s: %w", key, err)
	}
	return value, true, nil
}

// value 读取 key 的类型化值：库中有行则 JSON 解码，无行则取默认值或报缺。
func (s *Settings) value(ctx context.Context, key SettingKey) (any, error) {
	spec, ok := specFor(key)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownSetting, key)
	}
	encoded, found, err := s.load(ctx, key)
	if err != nil {
		return nil, err
	}
	if !found {
		if spec.required || spec.optional {
			return nil, fmt.Errorf("%w: %s", ErrSettingMissing, key)
		}
		// 默认值走同一条校验路径，注册表里写错默认值会在这里暴露。
		return parseSetting(key, spec.def)
	}
	switch spec.kind {
	case kindInt:
		var n int
		if err := json.Unmarshal([]byte(encoded), &n); err != nil {
			return nil, fmt.Errorf("解码配置 %s: %w", key, err)
		}
		return n, nil
	case kindBool:
		var b bool
		if err := json.Unmarshal([]byte(encoded), &b); err != nil {
			return nil, fmt.Errorf("解码配置 %s: %w", key, err)
		}
		return b, nil
	default: // 其余 kind 都存 JSON 字符串
		var str string
		if err := json.Unmarshal([]byte(encoded), &str); err != nil {
			return nil, fmt.Errorf("解码配置 %s: %w", key, err)
		}
		return str, nil
	}
}

// load 读 key 的已编码值。found 为 false 表示库中无该行。
func (s *Settings) load(ctx context.Context, key SettingKey) (encoded string, found bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT value_json FROM settings WHERE key = ?`, string(key)).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("读取配置 %s: %w", key, err)
	}
	return encoded, true, nil
}

// settingKind 决定校验与 JSON 编码方式。
type settingKind int

const (
	kindString settingKind = iota // 非空字符串（必填项）
	kindURL                       // 空，或带 scheme 的绝对 URL
	kindSecret                    // 至少 minSecretBytes 字节
	kindInt                       // [min, max] 闭区间整数
	kindBool                      // true / false
)

// settingSpec 是一个 key 的注册表条目。
//
// required 为 false 时 def 是默认值的原始文本，读取时会过 parseSetting；
// required 为 true 时 def 无意义。
// optional 为 true 表示「库中无行」本身是一种合法状态（覆盖项：空 = 继承/用全局），
// 只能通过 OptionalInt 读到 found=false；Int / String / Bool 遇到它返回 ErrSettingMissing。
type settingSpec struct {
	kind     settingKind
	required bool
	optional bool
	def      string
	min, max int
}

// specFor 返回 key 的注册表条目。switch 返回值而非包级 map：
// 无包级可变状态，调用方拿到副本。
func specFor(key SettingKey) (settingSpec, bool) {
	switch key {
	case KeyStrmPrefix:
		return settingSpec{kind: kindURL}, true
	case KeyStreamSecret:
		return settingSpec{kind: kindSecret, required: true}, true
	case KeySegTargetSec:
		return settingSpec{kind: kindInt, def: "10", min: 4, max: 30}, true
	case KeyWebDAVUser:
		return settingSpec{kind: kindString, required: true}, true
	case KeyWebDAVPass:
		return settingSpec{kind: kindString, required: true}, true
	case KeyCacheDir:
		// 缓存根目录：海报/分片/关键帧索引落 <cache_dir>/{cover,seg,index}/…；写盘时按需 MkdirAll。
		return settingSpec{kind: kindString, def: "./data/cache"}, true
	case KeyCacheMaxMB:
		// 分片缓存上限（MB）：0 = 不按大小淘汰，只受 cache_lru_days 约束。
		return settingSpec{kind: kindInt, def: "8192", min: 0, max: 1048576}, true
	case KeyCacheLRUDays:
		// 超过 N 天未命中的缓存条目（三种 kind 都算）删除。
		return settingSpec{kind: kindInt, def: "30", min: 1, max: 365}, true
	case KeyRefreshIntervalSec:
		// 全局刷新间隔：所有源共用的默认值（docs/SCHEDULER-MODEL.md「配置」）。
		return settingSpec{kind: kindInt, def: "3600", min: 300, max: 86400}, true
	case KeyRefreshBatch:
		// 每轮存量刷新部数；0 = 关闭存量轮转（只剩枚举 + 新增 + 手动）。
		return settingSpec{kind: kindInt, def: "1000", min: 0, max: 10000}, true
	case KeyRefreshTimeoutSec:
		// 单轮预算（context.WithTimeout）。
		return settingSpec{kind: kindInt, def: "900", min: 60, max: 86400}, true
	}
	return pluginSpecFor(string(key))
}

// pluginSpecFor 解析插件配置键 plugin.<源>.<项>。
//
// 源的取值：底层插件的 Name()（红果是 "web"），沿用 1.0 的 provider 标识；只允许
// 小写字母、数字、下划线、短横线，首字符不能是分隔符——避免键里出现第二个分隔点或空段。
func pluginSpecFor(key string) (settingSpec, bool) {
	rest, ok := strings.CutPrefix(key, "plugin.")
	if !ok {
		return settingSpec{}, false
	}
	source, item, ok := strings.Cut(rest, ".")
	if !ok || !validSourceName(source) {
		return settingSpec{}, false
	}
	switch item {
	case PluginItemEnabled:
		// 内置源默认启用：升级到 2.0 后红果不需要先手工开一下。
		return settingSpec{kind: kindBool, def: "true"}, true
	case PluginItemBaseURL:
		// 空 = 用插件内置默认地址，与「未设置」等价。
		return settingSpec{kind: kindURL}, true
	case PluginItemRequestTimeout:
		return settingSpec{kind: kindInt, def: "30", min: 5, max: 300}, true
	case PluginItemRefreshInterval:
		// 无默认值：空 = 继承全局 refresh_interval_sec，因此没有行就是「未设置」，
		// 不用某个数字冒充继承（用 OptionalInt 读）。
		return settingSpec{kind: kindInt, optional: true, min: 300, max: 86400}, true
	}
	return settingSpec{}, false
}

// validSourceName 校验插件名能否作为配置键里的一段。
func validSourceName(name string) bool {
	if name == "" {
		return false
	}
	for i, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '_' || c == '-':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// parseSetting 校验并规范化 raw，返回可 JSON 编码的类型化值（string 或 int）。
//
// 纯函数：不碰数据库、不碰文件，可直接单测。
func parseSetting(key SettingKey, raw string) (any, error) {
	spec, ok := specFor(key)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownSetting, key)
	}
	switch spec.kind {
	case kindURL:
		// B2：尾 `/` 写入时归一化。URL 类键都是按字符串拼接用的
		// （`{strm_prefix}/video/…`、`{base_url}{path}`），留着尾斜杠会拼出
		// `http://h:1//video/…`（MKACG-617 实测）。同一地址的两种写法归一成一种，
		// 不算「纠正非法值」——非法值仍然报错、不猜。
		raw = strings.TrimRight(raw, "/")
		if raw == "" {
			return "", nil
		}
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: %s 不是合法 URL: %v", ErrInvalidSetting, key, err)
		}
		if !u.IsAbs() {
			return nil, fmt.Errorf("%w: %s 必须是带 scheme 的绝对 URL", ErrInvalidSetting, key)
		}
		return raw, nil
	case kindSecret:
		if len(raw) < minSecretBytes {
			return nil, fmt.Errorf("%w: %s 至少 %d 字节，实际 %d", ErrInvalidSetting, key, minSecretBytes, len(raw))
		}
		return raw, nil
	case kindString:
		if raw == "" {
			return nil, fmt.Errorf("%w: %s 不能为空", ErrInvalidSetting, key)
		}
		return raw, nil
	case kindBool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: %s 不是布尔值: %q", ErrInvalidSetting, key, raw)
		}
		return b, nil
	case kindInt:
		n, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("%w: %s 不是整数: %q", ErrInvalidSetting, key, raw)
		}
		if n < spec.min || n > spec.max {
			return nil, fmt.Errorf("%w: %s 必须在 %d–%d 之间，实际 %d", ErrInvalidSetting, key, spec.min, spec.max, n)
		}
		return n, nil
	default:
		return nil, fmt.Errorf("%w: %s 的类型未实现", ErrInvalidSetting, key)
	}
}
