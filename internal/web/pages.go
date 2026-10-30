package web

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"path"

	"github.com/justforlxz/drama-hub/internal/httpx"
)

// assets 是三页界面所需的模板与静态资源，随二进制嵌入（ADR-0002：单二进制、
// 无前端构建步骤、没有 package.json）。
//
//go:embed templates/*.html static/*
var assets embed.FS

// pageMeta 是一个页面的模板文件与浏览器标题。
type pageMeta struct {
	file  string
	title string
}

// pageMetaOf 返回页面的模板与标题。switch 而不是包级 map：不留包级可变状态。
func pageMetaOf(page string) (pageMeta, bool) {
	switch page {
	case "dramas":
		return pageMeta{file: "templates/dramas.html", title: "剧目"}, true
	case "merges":
		return pageMeta{file: "templates/merges.html", title: "合并编排"}, true
	case "status":
		return pageMeta{file: "templates/status.html", title: "同步与缓存"}, true
	default:
		return pageMeta{}, false
	}
}

// pageData 是模板的渲染入参：Page 同时用于高亮导航与 body 的 data-page，
// 页面 JS 靠 body[data-page] 决定挂哪一段逻辑。
type pageData struct {
	Page  string
	Title string
}

// handleDramasPage 处理 GET /（剧目列表与增删）。
func (s *Server) handleDramasPage(w http.ResponseWriter, r *http.Request) {
	s.servePage(w, r, "dramas")
}

// handleMergesPage 处理 GET /merges（合并编排）。
func (s *Server) handleMergesPage(w http.ResponseWriter, r *http.Request) {
	s.servePage(w, r, "merges")
}

// handleStatusPage 处理 GET /status（同步与缓存看板）。
func (s *Server) handleStatusPage(w http.ResponseWriter, r *http.Request) {
	s.servePage(w, r, "status")
}

// servePage 渲染一个页面。模板每次请求解析：三页总共几 KB，单用户管理面下这点
// 开销可以忽略，换来的是不持有包级模板对象。
func (s *Server) servePage(w http.ResponseWriter, r *http.Request, page string) {
	meta, ok := pageMetaOf(page)
	if !ok {
		http.NotFound(w, r)
		return
	}
	tpl, err := template.ParseFS(assets, "templates/base.html", meta.file)
	if err != nil {
		httpx.WriteError(w, fmt.Errorf("解析页面模板 %s: %w", meta.file, err))
		return
	}
	buf := &bytes.Buffer{}
	if err := tpl.ExecuteTemplate(buf, "base", pageData{Page: page, Title: meta.title}); err != nil {
		httpx.WriteError(w, fmt.Errorf("渲染页面 %s: %w", meta.file, err))
		return
	}
	writeBody(w, http.StatusOK, "text/html; charset=utf-8", buf.Bytes())
}

// handleStatic 提供内嵌的样式与脚本。
//
// `{file}` 只匹配单个路径段（不含 `/`），因此不存在目录穿越；只放行 .css / .js
// 两种后缀，其余按 404 处理 —— 管理面没有其它静态资源。
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	contentType := ""
	switch path.Ext(name) {
	case ".css":
		contentType = "text/css; charset=utf-8"
	case ".js":
		contentType = "text/javascript; charset=utf-8"
	default:
		http.NotFound(w, r)
		return
	}
	body, err := assets.ReadFile("static/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	writeBody(w, http.StatusOK, contentType, body)
}
