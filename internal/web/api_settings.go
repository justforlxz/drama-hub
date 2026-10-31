package web

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/justforlxz/drama-hub/internal/httpx"
	"github.com/justforlxz/drama-hub/internal/store"
)

// settingsRequest 是 PATCH /api/settings 的请求体：key → 原始值。
//
// 值一律按字符串给（与 settings 的文本形态一致）：数字与布尔写成 "4096" / "false"。
// 校验与归一化只走 store 的注册表那一套（specFor + parseSetting），本包不复制第二份
// 规则 —— 页面与接口的校验因此不可能不一致。
type settingsRequest struct {
	Settings map[string]string `json:"settings"`
}

// settingsView 是写入后的归一化结果。
//
// 密码与出流密钥不回显（值为 null）：它们只需要证明「写进去了」，回显等于把凭据多抄
// 一份到浏览器与会话记录里（docs/DEV-STANDARDS.md「日志」同样禁止记 token 与密码）。
type settingsView struct {
	Settings map[string]any `json:"settings"`
}

// handlePatchSettings 处理 PATCH /api/settings：按 key 批量写配置，返回归一化后的值。
//
// 整批要么全写、要么一行不写（store.Settings.SetMany 一个事务）：未知键与非法值都是
// 400，库里不动。写操作本身由 ServeHTTP 的令牌闸门保护（DRAMA_HUB_ADMIN_TOKEN）。
func (s *Server) handlePatchSettings(w http.ResponseWriter, r *http.Request) {
	var req settingsRequest
	if err := decodeJSON(w, r, &req); err != nil {
		httpx.WriteError(w, err)
		return
	}
	if len(req.Settings) == 0 {
		httpx.WriteError(w, fmt.Errorf("%w: settings 里没有任何配置项", httpx.ErrInvalid))
		return
	}
	values := make(map[store.SettingKey]string, len(req.Settings))
	for key, raw := range req.Settings {
		values[store.SettingKey(key)] = raw
	}
	written, err := s.settings.SetMany(r.Context(), values)
	if err != nil {
		httpx.WriteError(w, mapSettingErr(err))
		return
	}
	out := make(map[string]any, len(written))
	for key, value := range written {
		if isSecretSetting(key) {
			out[string(key)] = nil
			continue
		}
		out[string(key)] = value
	}
	writeJSON(w, http.StatusOK, settingsView{Settings: out})
}

// mapSettingErr 把 settings 的哨兵接到传输层：未知键与非法值都是「请求本身无效」→ 400，
// 错误文本原样带回（页面直接显示「必须在 0–1048576 之间」这类原因）。
func mapSettingErr(err error) error {
	switch {
	case errors.Is(err, store.ErrUnknownSetting), errors.Is(err, store.ErrInvalidSetting):
		return fmt.Errorf("%w: %v", httpx.ErrInvalid, err)
	default:
		return err
	}
}

// isSecretSetting 判断一个键的值是否不该回显。
func isSecretSetting(key store.SettingKey) bool {
	switch key {
	case store.KeyWebDAVPass, store.KeyStreamSecret:
		return true
	}
	return false
}
