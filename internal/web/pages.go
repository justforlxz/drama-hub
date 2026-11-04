package web

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"path"
	"strconv"

	"github.com/justforlxz/drama-hub/internal/httpx"
	"github.com/justforlxz/drama-hub/internal/store"
)

// assets 是管理页面所需的模板与静态资源，随二进制嵌入（ADR-0002：单二进制、
// 无前端构建步骤、没有 package.json）。
//
//go:embed templates/*.html static/*
var assets embed.FS

// pageMeta 是一页的模板与标题。页面集合只由 pages 决定：未收录的路径一律
// 404（不做 catch-all），所以这张表就是「后台有哪些页面」的唯一答案。
type pageMeta struct {
	File  string
	Title string
}

var pages = map[string]pageMeta{
	"overview":   {File: "templates/overview.html", Title: "概览"},
	"library":    {File: "templates/library.html", Title: "剧库"},
	"categories": {File: "templates/categories.html", Title: "分类"},
	"settings":   {File: "templates/settings.html", Title: "设置"},
}

// dramaPage 是剧详情页。它不出现在 pages 里：路径带 {id}，页面名固定是 drama，
// 导航高亮剧库（Nav 与 Page 因此分开）。
var dramaPage = pageMeta{File: "templates/drama.html", Title: "剧详情"}

// pageMetaOf 查页面集合。调用方只传本包的页面名常量，未登记路径由路由表兜底 404。
func pageMetaOf(page string) pageMeta {
	return pages[page]
}

// pageData 是模板的渲染入参：Page 用于 body 的 data-page（页面 JS 靠它决定挂哪段
// 逻辑），Nav 用于导航高亮（剧详情页高亮剧库，两者可以不同），DramaID 只有剧详情
// 页非空（路径参数原样交给页面 JS）。
type pageData struct {
	Page    string
	Nav     string
	Title   string
	DramaID string
}

// handlePage 渲染一页管理界面（templates/base.html + 该页模板）。
func (s *Server) handlePage(page string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		meta := pageMetaOf(page)
		s.renderPage(w, meta, pageData{Page: page, Nav: page, Title: meta.Title})
	}
}

// handleDramaPage 渲染剧详情页。id 不是正整数、或库里没有这部剧，都按 404 处理：
// 页面集合之外的东西不渲染（与未登记路径一致），也让「剧不存在」这个状态可见。
func (s *Server) handleDramaPage(w http.ResponseWriter, r *http.Request) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	if _, err := s.st.DramaByID(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		httpx.WriteError(w, fmt.Errorf("读剧 %s: %w", raw, err))
		return
	}
	s.renderPage(w, dramaPage, pageData{
		Page: "drama", Nav: "library", Title: dramaPage.Title, DramaID: raw,
	})
}

// renderPage 解析并执行整页模板。
//
// 模板每次请求解析：整页几 KB，单用户管理面下这点开销可以忽略，换来的是不持有
// 包级模板对象。
func (s *Server) renderPage(w http.ResponseWriter, meta pageMeta, data pageData) {
	tpl, err := template.ParseFS(assets, "templates/base.html", meta.File)
	if err != nil {
		httpx.WriteError(w, fmt.Errorf("解析页面模板 %s: %w", meta.File, err))
		return
	}
	buf := &bytes.Buffer{}
	if err := tpl.ExecuteTemplate(buf, "base", data); err != nil {
		httpx.WriteError(w, fmt.Errorf("渲染页面 %s: %w", meta.File, err))
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
