package httpx

import (
	"encoding/json"
	"errors"
	"net/http"
)

// HTTP 边界的传输层错误类别。边界包（web / stream / webdav）在返回前用
// fmt.Errorf("%w: 详情", httpx.ErrXxx) 打标；domain 包保留自己的包内哨兵，
// 不 import 本包。
var (
	// ErrNotFound 表示请求的资源不存在，映射 404。
	ErrNotFound = errors.New("资源不存在")
	// ErrInvalid 表示请求本身无效（参数、范围、格式），映射 400。
	ErrInvalid = errors.New("请求无效")
	// ErrConflict 表示请求与当前状态冲突（同一资源正在被处理），映射 409。
	// 目前只有手动触发撞上正在跑的一轮（docs/SCHEDULER-MODEL.md「手动触发」）。
	ErrConflict = errors.New("与当前状态冲突")
	// ErrUnauthorized 表示签名或凭据不通过，映射 401。出流（ADR-0003）与
	// WebDAV Basic 认证共用；本 token 不是 HTTP 认证方案，因此不写
	// WWW-Authenticate。
	ErrUnauthorized = errors.New("未授权")
	// ErrUpstream 表示上游（数据源 / CDN）失败，映射 502。
	ErrUpstream = errors.New("上游失败")
	// ErrUnavailable 表示依赖暂不可用（索引未完成等），映射 503。
	ErrUnavailable = errors.New("暂不可用")
)

// StatusCode 把错误映射为 HTTP 状态码：六个哨兵分别 404 / 400 / 401 / 409 / 502 / 503，
// 未命中任何哨兵返回 500。用 errors.Is 判定，因此支持 %w 包装；各哨兵互不
// 包含，判定顺序不影响结果。
func StatusCode(err error) int {
	switch {
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrInvalid):
		return http.StatusBadRequest
	case errors.Is(err, ErrConflict):
		return http.StatusConflict
	case errors.Is(err, ErrUnauthorized):
		return http.StatusUnauthorized
	case errors.Is(err, ErrUpstream):
		return http.StatusBadGateway
	case errors.Is(err, ErrUnavailable):
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// errorBody 是统一错误响应体。
type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// errorCode 把状态码映射为响应体的 code 字段。
func errorCode(status int) string {
	switch status {
	case http.StatusNotFound:
		return "not_found"
	case http.StatusBadRequest:
		return "invalid"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusConflict:
		return "conflict"
	case http.StatusBadGateway:
		return "upstream"
	case http.StatusServiceUnavailable:
		return "unavailable"
	default:
		return "internal"
	}
}

// WriteError 按 StatusCode(err) 写出统一错误响应：设置
// Content-Type: application/json; charset=utf-8，先 WriteHeader 再写
// {"code":"<kind>","message":"<err.Error()>"}。
//
// 本函数刻意不写日志：每请求一条访问日志的职责归 RequestLogger，这里另行记录
// 会让失败请求出现两条日志。err 必须非 nil（调用方保证）；错误详情只进响应体，
// 因此调用方须遵守「错误里不得带 token / 密码」的约定。
func WriteError(w http.ResponseWriter, err error) {
	status := StatusCode(err)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Code: errorCode(status), Message: err.Error()})
}
