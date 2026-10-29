package vfs

import "errors"

// ErrNotFound 表示请求的路径不是本树里的一个文件。
//
// 覆盖：不在 短剧/ 下、目录路径当文件渲染、段数不符、扩展名不是 .strm/.nfo、
// 剧目或集在库里不存在。调用方（M4 webdav）用 errors.Is 判定并映射为 404。
var ErrNotFound = errors.New("虚拟文件不存在")
