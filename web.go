package main

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

//go:embed web
var webFS embed.FS

// registerStaticFiles 将嵌入的前端 dist 文件挂载到 gin，所有非 /api 路由
// 均尝试直接服务静态文件，文件不存在时回退到 index.html（SPA 客户端路由）
func registerStaticFiles(r *gin.Engine) {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		return
	}
	fileServer := http.FileServer(http.FS(sub))

	r.NoRoute(func(c *gin.Context) {
		reqPath := strings.TrimPrefix(c.Request.URL.Path, "/")
		if reqPath == "" {
			reqPath = "index.html"
		}

		// 尝试打开对应文件
		f, err := sub.Open(reqPath)
		if err == nil {
			stat, statErr := f.Stat()
			f.Close()
			// 是普通文件则直接服务
			if statErr == nil && !stat.IsDir() {
				fileServer.ServeHTTP(c.Writer, c.Request)
				return
			}
		}

		// SPA fallback：路径不对应任何文件时返回 index.html
		c.Request.URL.Path = "/"
		fileServer.ServeHTTP(c.Writer, c.Request)
	})
}
