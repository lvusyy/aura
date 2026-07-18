// Package console 托管嵌入 controller 二进制的 aura-console 前端 SPA 产物（M8）。
//
// 前端 Vite 构建产物置于本包 dist/ 子目录（TASK-004 构建流程 outDir / TASK-012 部署复制），经 //go:embed
// 编译进二进制，无外部静态文件依赖。未知路径回退 index.html 支撑 SPA client-side 路由深链刷新不 404。
package console

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// distFS 嵌入前端构建产物。all: 前缀纳入 . / _ 前缀文件（Vite 可能产 .vite/ 等）。骨架阶段 dist/ 仅
// 占位 index.html（TASK-004 真产物覆盖）——embed 要求目录编译期非空，故占位不可缺。
//
//go:embed all:dist
var distFS embed.FS

// mountPrefix 是 SPA 部署前缀。前端 vite base 与 react-router basename 均为 /console（console-design §2），
// 产物 index.html 以 /console/assets/* 绝对路径引用资源——服务端须以同前缀托管，否则资源请求落 index.html
// 回退（MIME=text/html）致浏览器拒执行脚本、console 白屏。
const mountPrefix = "/console"

// Handler 返回托管嵌入 SPA 的 http.Handler，统一挂 /console/ 前缀（对齐前端 base/basename）：
//   - /console/<资源> 命中真实文件 → FileServer（StripPrefix 去 /console 后于 dist/ 根定位）直出，正确 Content-Type；
//   - /console 或 /console/ 根、/console/<深链>（前端路由，无对应文件）→ 回退 index.html（200）刷新不 404；
//   - 前缀外路径（如根 /）→ 302 重定向到 /console/，给出唯一规范入口。
//
// dist/ 缺 index.html（embed 破损/构建产物异常）时返回 error，由装配方 fatal。
func Handler() (http.Handler, error) {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return nil, err
	}
	return handlerFrom(sub)
}

// handlerFrom 按给定产物根构造 SPA handler。与 Handler 拆分使缓存头/回退逻辑可经
// fstest.MapFS 注入测试——仓库内 embed dist 仅占位 index.html（真产物由 console 构建落位），
// 不能作为 assets 用例的 fixture。
func handlerFrom(sub fs.FS) (http.Handler, error) {
	index, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		return nil, err
	}
	// StripPrefix 去 /console 后于 dist/ 根定位（资源实体在 dist/assets/*，产物引用 /console/assets/*）。
	fileServer := http.StripPrefix(mountPrefix, http.FileServer(http.FS(sub)))
	serveIndex := func(w http.ResponseWriter) {
		// index.html 引用内容 hash 命名的 bundle（assets/index-<hash>.js）。若浏览器启发式缓存旧 index.html，
		// 刷新将继续引用旧 bundle，前端新版永不生效。no-cache 令浏览器每次使用前重新校验（无 validator 时即
		// 完整重取），保证刷新拿到最新 index.html 与其 bundle 引用。根 /console/ 与 SPA 深链回退均经此，一处覆盖。
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(index)
	}
	// serveAsset 包裹 FileServer：assets/* 为内容 hash 命名，内容变则文件名（URL）变，故可长缓存 + immutable
	// —— 缓存有效期内浏览器跳过校验请求，hash 变更自然拉新，零陈旧风险；非 assets 实体不加长缓存。
	serveAsset := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, mountPrefix+"/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		fileServer.ServeHTTP(w, r)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 归一化路径：path.Clean 消解 ../ 防越界（io/fs 亦拒非法路径）。
		clean := path.Clean("/" + r.URL.Path)
		// 前缀外路径（根 / 等）→ 重定向到规范入口 /console/。
		if clean != mountPrefix && !strings.HasPrefix(clean, mountPrefix+"/") {
			http.Redirect(w, r, mountPrefix+"/", http.StatusFound)
			return
		}
		// /console 或 /console/ 根 → SPA 首页。
		rel := strings.TrimPrefix(clean, mountPrefix+"/")
		if clean == mountPrefix || rel == "" {
			serveIndex(w)
			return
		}
		// 前缀内路径：命中真实资源 → FileServer（经 serveAsset 附长缓存头）直出；未命中（前端路由深链）→
		// 回退 index.html 保 200。
		if _, statErr := fs.Stat(sub, rel); statErr != nil {
			serveIndex(w)
			return
		}
		serveAsset.ServeHTTP(w, r)
	}), nil
}
