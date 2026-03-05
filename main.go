package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// --- config ---

var (
	listenAddr string
	tlsCert    string
	tlsKey     string
	daemonize  bool
	logFile    string
)

const (
	dockerHub = "registry-1.docker.io"
)

var routes = map[string]string{
	"quay":       "quay.io",
	"gcr":        "gcr.io",
	"k8s-gcr":    "k8s.gcr.io",
	"k8s":        "registry.k8s.io",
	"ghcr":       "ghcr.io",
	"cloudsmith": "docker.cloudsmith.io",
	"nvcr":       "nvcr.io",
}

var blockedUAs = []string{"netcraft"}

// --- regex ---

var (
	v2ShortPathRegex = regexp.MustCompile(`^/v2/[^/]+/[^/]+/[^/]+$`)
	v2LibraryRegex   = regexp.MustCompile(`^/v2/library`)
)

// --- http clients ---

var registryClient = &http.Client{
	Timeout: 300 * time.Second,
	Transport: &http.Transport{
		TLSClientConfig:       &tls.Config{},
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   50,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	},
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

var downloadClient = &http.Client{
	Timeout: 600 * time.Second,
	Transport: &http.Transport{
		TLSClientConfig:       &tls.Config{},
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   50,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	},
}

// --- main ---

func main() {
	flag.StringVar(&listenAddr, "addr", ":5000", "监听地址")
	flag.StringVar(&tlsCert, "tls-cert", "", "TLS 证书路径")
	flag.StringVar(&tlsKey, "tls-key", "", "TLS 私钥路径")
	flag.BoolVar(&daemonize, "d", false, "后台守护进程模式")
	flag.StringVar(&logFile, "log", "docker-proxy.log", "日志文件路径")
	flag.Parse()

	if daemonize {
		runDaemon()
		return
	}
	if os.Getenv("_DOCKER_PROXY_CHILD") == "1" {
		setupLogging()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleRequest)

	server := &http.Server{
		Addr:         listenAddr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 300 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		<-ch
		log.Println("正在关闭...")
		server.Close()
	}()

	if tlsCert != "" && tlsKey != "" {
		log.Printf("Docker 代理已启动 (HTTPS) %s\n", listenAddr)
		if err := server.ListenAndServeTLS(tlsCert, tlsKey); err != http.ErrServerClosed {
			log.Fatalf("服务异常: %v", err)
		}
	} else {
		log.Printf("Docker 代理已启动 (HTTP) %s\n", listenAddr)
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("服务异常: %v", err)
		}
	}
}

// --- daemon ---

func runDaemon() {
	var args []string
	for _, a := range os.Args[1:] {
		if a != "-d" {
			args = append(args, a)
		}
	}
	proc, err := os.StartProcess(os.Args[0], append([]string{os.Args[0]}, args...), &os.ProcAttr{
		Dir:   ".",
		Env:   append(os.Environ(), "_DOCKER_PROXY_CHILD=1"),
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
		Sys:   &syscall.SysProcAttr{Setsid: true},
	})
	if err != nil {
		log.Fatalf("启动守护进程失败: %v", err)
	}
	fmt.Printf("Docker 代理已在后台启动, PID: %d\n", proc.Pid)
	if f, err := os.Create("docker-proxy.pid"); err == nil {
		fmt.Fprintf(f, "%d\n", proc.Pid)
		f.Close()
	}
	os.Exit(0)
}

func setupLogging() {
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Fatalf("无法打开日志文件: %v", err)
	}
	log.SetOutput(f)
	os.Stdout = f
	os.Stderr = f
}

// --- upstream resolution ---
// Mirrors the Worker's routing logic: ns param > hubhost param > Host header prefix

func resolveUpstream(r *http.Request) (hubHost string, isDockerHub bool) {
	if ns := r.URL.Query().Get("ns"); ns != "" {
		if ns == "docker.io" {
			return dockerHub, true
		}
		return ns, false
	}
	hostname := r.URL.Query().Get("hubhost")
	if hostname == "" {
		hostname = r.Host
	}
	hostTop := strings.Split(hostname, ".")[0]
	if u, ok := routes[hostTop]; ok {
		return u, false
	}
	return dockerHub, true
}

func isBlockedUA(ua string) bool {
	for _, b := range blockedUAs {
		if strings.Contains(ua, b) {
			return true
		}
	}
	return false
}

// --- main request router ---

func handleRequest(w http.ResponseWriter, r *http.Request) {
	log.Printf("[%s] %s %s%s", r.RemoteAddr, r.Method, r.URL.Path, qstr(r))

	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,PATCH,TRACE,DELETE,HEAD,OPTIONS")
		w.Header().Set("Access-Control-Max-Age", "1728000")
		w.WriteHeader(http.StatusOK)
		return
	}

	hubHost, isDockerHub := resolveUpstream(r)
	ua := strings.ToLower(r.Header.Get("User-Agent"))

	if isBlockedUA(ua) {
		serveNginxPage(w)
		return
	}

	path := r.URL.Path
	isBrowser := strings.Contains(ua, "mozilla")
	isV1Hub := strings.Contains(path, "/v1/search") || strings.Contains(path, "/v1/repositories")

	if isBrowser || isV1Hub {
		handleBrowser(w, r, hubHost, isDockerHub)
		return
	}

	switch {
	case path == "/v2/" || path == "/v2":
		handleV2Ping(w)
	case strings.HasPrefix(path, "/_auth/"):
		handleAuthProxy(w, r)
	case strings.HasPrefix(path, "/v2/"):
		handleV2(w, r, hubHost, isDockerHub)
	case path == "/health":
		handleHealth(w)
	default:
		proxyDirect(w, r, hubHost)
	}
}

// --- browser / search ---

func handleBrowser(w http.ResponseWriter, r *http.Request, hubHost string, isDockerHub bool) {
	path := r.URL.Path

	if path == "/" {
		if isDockerHub {
			serveSearchPage(w)
		} else {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprintf(w, "Docker Registry Proxy → %s", hubHost)
		}
		return
	}

	if strings.HasPrefix(path, "/v1/") {
		proxyBrowser(w, r, "index.docker.io")
		return
	}

	if isDockerHub {
		if q := r.URL.Query().Get("q"); strings.Contains(q, "library/") && q != "library/" {
			vals := r.URL.Query()
			vals.Set("q", strings.Replace(q, "library/", "", 1))
			r.URL.RawQuery = vals.Encode()
		}
		proxyBrowser(w, r, "hub.docker.com")
		return
	}

	proxyBrowser(w, r, hubHost)
}

func proxyBrowser(w http.ResponseWriter, r *http.Request, host string) {
	target := fmt.Sprintf("https://%s%s", host, r.URL.Path)
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	req, err := http.NewRequest(r.Method, target, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	copyAllHeaders(req.Header, r.Header)
	req.Header.Set("Host", host)

	resp, err := downloadClient.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	flushResponse(w, resp)
}

// --- /health ---

func handleHealth(w http.ResponseWriter) {
	type checkResult struct {
		Name    string `json:"name"`
		URL     string `json:"url"`
		Status  string `json:"status"`
		Latency string `json:"latency"`
		Detail  string `json:"detail,omitempty"`
	}

	checks := []struct {
		name string
		url  string
	}{
		{"auth.docker.io", "https://auth.docker.io/token?service=registry.docker.io&scope=repository:library/alpine:pull"},
		{"registry-1.docker.io", "https://registry-1.docker.io/v2/"},
		{"hub.docker.com", "https://hub.docker.com/"},
	}

	var results []checkResult
	for _, c := range checks {
		start := time.Now()
		req, _ := http.NewRequest("GET", c.url, nil)
		req.Header.Set("User-Agent", "docker-proxy/health-check")
		resp, err := registryClient.Do(req)
		elapsed := time.Since(start)

		r := checkResult{
			Name:    c.name,
			URL:     c.url,
			Latency: elapsed.Round(time.Millisecond).String(),
		}
		if err != nil {
			r.Status = "FAIL"
			r.Detail = err.Error()
		} else {
			resp.Body.Close()
			r.Status = fmt.Sprintf("HTTP %d", resp.StatusCode)
			if resp.StatusCode < 500 {
				r.Detail = "OK"
			}
		}
		results = append(results, r)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(map[string]any{
		"proxy":  "running",
		"time":   time.Now().Format(time.RFC3339),
		"listen": listenAddr,
		"checks": results,
	})
}

// --- /v2/ ping ---
// Returns 200 directly — proxy handles all auth internally.

func handleV2Ping(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("{}"))
}

// --- auth proxy ---
// 代理认证请求到上游认证服务，支持所有仓库

func handleAuthProxy(w http.ResponseWriter, r *http.Request) {
	// 路径格式: /_auth/{auth-host}/remaining-path
	trimmed := strings.TrimPrefix(r.URL.Path, "/_auth/")
	slashIdx := strings.Index(trimmed, "/")
	if slashIdx == -1 {
		http.Error(w, "invalid auth path", http.StatusBadRequest)
		return
	}
	authHost := trimmed[:slashIdx]
	remainingPath := trimmed[slashIdx:]

	target := fmt.Sprintf("https://%s%s", authHost, remainingPath)
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	req, err := http.NewRequest(r.Method, target, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set("Host", authHost)
	copySelectHeaders(req.Header, r.Header)
	if auth := r.Header.Get("Authorization"); auth != "" {
		req.Header.Set("Authorization", auth)
	}

	resp, err := registryClient.Do(req)
	if err != nil {
		log.Printf("auth 代理失败 (%s): %v", authHost, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	flushResponse(w, resp)
}

// rewriteAuthHeader 将上游 401 响应中的 Www-Authenticate 认证地址改写为代理地址
func rewriteAuthHeader(header string, r *http.Request, hubHost string) string {
	scheme := "http"
	if r.TLS != nil || tlsCert != "" {
		scheme = "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}
	proxyBase := scheme + "://" + r.Host + "/_auth"

	// Docker Hub: 认证服务在独立域名 auth.docker.io
	result := strings.Replace(header, "https://auth.docker.io", proxyBase+"/auth.docker.io", -1)
	// 通用仓库: 认证服务通常在同一域名
	if hubHost != dockerHub {
		result = strings.Replace(result, "https://"+hubHost, proxyBase+"/"+hubHost, -1)
	}

	return result
}

// --- /v2/<name>/... ---

func handleV2(w http.ResponseWriter, r *http.Request, hubHost string, isDockerHub bool) {
	path := r.URL.Path
	rawQuery := r.URL.RawQuery

	// Worker 逻辑: 如果 query 不含 %2F 但整体含 %3A, 在第一个 %3A 后插入 library%2F
	if isDockerHub && !containsCI(rawQuery, "%2F") {
		fullURI := path
		if rawQuery != "" {
			fullURI += "?" + rawQuery
		}
		if containsCI(fullURI, "%3A") {
			if fixed := fixEncodedLibrary(fullURI); fixed != fullURI {
				if qi := strings.Index(fixed, "?"); qi != -1 {
					path = fixed[:qi]
					rawQuery = fixed[qi+1:]
				} else {
					path = fixed
					rawQuery = ""
				}
				log.Printf("编码修正: %s -> %s", r.URL.RequestURI(), fixed)
			}
		}
	}

	// Docker Hub 官方镜像自动补 library/ 前缀
	if isDockerHub && v2ShortPathRegex.MatchString(path) && !v2LibraryRegex.MatchString(path) {
		if parts := strings.SplitN(path, "/v2/", 2); len(parts) == 2 {
			path = "/v2/library/" + parts[1]
			log.Printf("补全 library/: %s -> %s", r.URL.Path, path)
		}
	}

	target := fmt.Sprintf("https://%s%s", hubHost, path)
	if rawQuery != "" {
		target += "?" + rawQuery
	}

	req, err := http.NewRequest(r.Method, target, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	copySelectHeaders(req.Header, r.Header)
	req.Header.Set("Host", hubHost)

	if auth := r.Header.Get("Authorization"); auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if v := r.Header.Get("X-Amz-Content-Sha256"); v != "" {
		req.Header.Set("X-Amz-Content-Sha256", v)
	}

	resp, err := registryClient.Do(req)
	if err != nil {
		log.Printf("上游请求失败: %v", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if loc := resp.Header.Get("Location"); loc != "" && isRedirectCode(resp.StatusCode) {
		log.Printf("跟随重定向: %s", loc)
		handleCDNRedirect(w, r, loc)
		return
	}

	if resp.StatusCode == http.StatusUnauthorized {
		if authHeader := resp.Header.Get("Www-Authenticate"); authHeader != "" {
			rewritten := rewriteAuthHeader(authHeader, r, hubHost)
			resp.Header.Set("Www-Authenticate", rewritten)
			log.Printf("认证重写: %s -> %s", authHeader, rewritten)
		}
	}

	flushResponse(w, resp)
}

// --- CDN redirect (blob download) ---
// Mirrors Worker's httpHandler + proxy: copy all headers, delete Authorization, follow redirects.

func handleCDNRedirect(w http.ResponseWriter, orig *http.Request, location string) {
	req, err := http.NewRequest(orig.Method, location, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	copyAllHeaders(req.Header, orig.Header)
	req.Header.Del("Authorization")

	resp, err := downloadClient.Do(req)
	if err != nil {
		log.Printf("CDN 下载失败: %v", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Access-Control-Expose-Headers", "*")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "max-age=1500")
	w.Header().Del("Content-Security-Policy")
	w.Header().Del("Content-Security-Policy-Report-Only")
	w.Header().Del("Clear-Site-Data")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// --- fallback proxy ---

func proxyDirect(w http.ResponseWriter, r *http.Request, hubHost string) {
	target := fmt.Sprintf("https://%s%s", hubHost, r.URL.Path)
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	req, err := http.NewRequest(r.Method, target, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	copySelectHeaders(req.Header, r.Header)
	req.Header.Set("Host", hubHost)
	if auth := r.Header.Get("Authorization"); auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if v := r.Header.Get("X-Amz-Content-Sha256"); v != "" {
		req.Header.Set("X-Amz-Content-Sha256", v)
	}

	resp, err := registryClient.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if loc := resp.Header.Get("Location"); loc != "" && isRedirectCode(resp.StatusCode) {
		handleCDNRedirect(w, r, loc)
		return
	}

	flushResponse(w, resp)
}


// --- helpers ---

func copySelectHeaders(dst, src http.Header) {
	for _, k := range []string{
		"User-Agent", "Accept", "Accept-Language", "Accept-Encoding",
		"Connection", "Cache-Control", "If-None-Match", "If-Modified-Since",
	} {
		if v := src.Get(k); v != "" {
			dst.Set(k, v)
		}
	}
}

func copyAllHeaders(dst, src http.Header) {
	for k, vv := range src {
		if strings.EqualFold(k, "Host") {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func flushResponse(w http.ResponseWriter, resp *http.Response) {
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Expose-Headers", "*")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func isRedirectCode(code int) bool {
	return code == 301 || code == 302 || code == 303 || code == 307 || code == 308
}

// Worker 逻辑: 当 query 无 %2F 但 URL 有 %3A 时，在第一个 %3A 后面(且后面有 &)插入 library%2F
func fixEncodedLibrary(uri string) string {
	lower := strings.ToLower(uri)
	idx := strings.Index(lower, "%3a")
	if idx == -1 {
		return uri
	}
	rest := uri[idx+3:]
	if strings.Contains(rest, "&") {
		return uri[:idx+3] + "library%2F" + rest
	}
	return uri
}

func containsCI(s, substr string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(substr))
}

func qstr(r *http.Request) string {
	if r.URL.RawQuery != "" {
		return "?" + r.URL.RawQuery
	}
	return ""
}

// --- pages ---

func serveNginxPage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, `<!DOCTYPE html>
<html>
<head><title>Welcome to nginx!</title>
<style>body{width:35em;margin:0 auto;font-family:Tahoma,Verdana,Arial,sans-serif;}</style>
</head>
<body>
<h1>Welcome to nginx!</h1>
<p>If you see this page, the nginx web server is successfully installed and working. Further configuration is required.</p>
<p>For online documentation and support please refer to <a href="http://nginx.org/">nginx.org</a>.<br/>
Commercial support is available at <a href="http://nginx.com/">nginx.com</a>.</p>
<p><em>Thank you for using nginx.</em></p>
</body></html>`)
}

func serveSearchPage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, `<!DOCTYPE html>
<html>
<head>
	<title>Docker Hub 镜像搜索</title>
	<meta charset="UTF-8">
	<meta name="viewport" content="width=device-width, initial-scale=1.0">
	<style>
	:root {
		--primary-color: #0066ff;
		--primary-dark: #0052cc;
		--gradient-start: #1a90ff;
		--gradient-end: #003eb3;
		--text-color: #ffffff;
		--transition-time: 0.3s;
	}
	* { box-sizing: border-box; margin: 0; padding: 0; }
	body {
		font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
		display: flex; flex-direction: column; justify-content: center; align-items: center;
		min-height: 100vh; margin: 0;
		background: linear-gradient(135deg, var(--gradient-start) 0%, var(--gradient-end) 100%);
		padding: 20px; color: var(--text-color); overflow-x: hidden;
	}
	.container {
		text-align: center; width: 100%; max-width: 800px; padding: 20px; margin: 0 auto;
		display: flex; flex-direction: column; justify-content: center; min-height: 60vh;
		animation: fadeIn 0.8s ease-out;
	}
	@keyframes fadeIn { from { opacity: 0; transform: translateY(20px); } to { opacity: 1; transform: translateY(0); } }
	.logo { margin-bottom: 20px; animation: float 6s ease-in-out infinite; }
	@keyframes float { 0%, 100% { transform: translateY(0); } 50% { transform: translateY(-10px); } }
	.logo:hover { transform: scale(1.08) rotate(5deg); }
	.logo svg { filter: drop-shadow(0 5px 15px rgba(0,0,0,0.2)); }
	.title {
		color: var(--text-color); font-size: 2.3em; margin-bottom: 10px;
		text-shadow: 0 2px 10px rgba(0,0,0,0.2); font-weight: 700; letter-spacing: -0.5px;
	}
	.subtitle {
		color: rgba(255,255,255,0.9); font-size: 1.1em; margin-bottom: 25px;
		max-width: 600px; margin-left: auto; margin-right: auto; line-height: 1.4;
	}
	.search-container {
		display: flex; align-items: stretch; width: 100%; max-width: 600px; margin: 0 auto;
		height: 55px; box-shadow: 0 10px 25px rgba(0,0,0,0.15); border-radius: 12px; overflow: hidden;
	}
	#search-input {
		flex: 1; padding: 0 20px; font-size: 16px; border: none; outline: none;
		transition: all var(--transition-time) ease; height: 100%;
	}
	#search-input:focus { padding-left: 25px; }
	#search-button {
		width: 60px; background-color: var(--primary-color); border: none; cursor: pointer;
		transition: all var(--transition-time) ease; height: 100%;
		display: flex; align-items: center; justify-content: center;
	}
	#search-button svg { transition: transform 0.3s ease; stroke: white; }
	#search-button:hover { background-color: var(--primary-dark); }
	#search-button:hover svg { transform: translateX(2px); }
	.tips { color: rgba(255,255,255,0.8); margin-top: 20px; font-size: 0.9em; }
	@media (max-width: 768px) { .title { font-size: 2em; } .search-container { height: 50px; } }
	@media (max-width: 480px) { .title { font-size: 1.7em; } .search-container { height: 45px; } #search-button { width: 50px; } }
	</style>
</head>
<body>
	<div class="container">
		<div class="logo">
			<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 18" fill="#ffffff" width="110" height="85">
				<path d="M23.763 6.886c-.065-.053-.673-.512-1.954-.512-.32 0-.659.03-1.01.087-.248-1.703-1.651-2.533-1.716-2.57l-.345-.2-.227.328a4.596 4.596 0 0 0-.611 1.433c-.23.972-.09 1.884.403 2.666-.596.331-1.546.418-1.744.42H.752a.753.753 0 0 0-.75.749c-.007 1.456.233 2.864.692 4.07.545 1.43 1.355 2.483 2.409 3.13 1.181.725 3.104 1.14 5.276 1.14 1.016 0 2.03-.092 2.93-.266 1.417-.273 2.705-.742 3.826-1.391a10.497 10.497 0 0 0 2.61-2.14c1.252-1.42 1.998-3.005 2.553-4.408.075.003.148.005.221.005 1.371 0 2.215-.55 2.68-1.01.505-.5.685-.998.704-1.053L24 7.076l-.237-.19Z"></path>
				<path d="M2.216 8.075h2.119a.186.186 0 0 0 .185-.186V6a.186.186 0 0 0-.185-.186H2.216A.186.186 0 0 0 2.031 6v1.89c0 .103.083.186.185.186Zm2.92 0h2.118a.185.185 0 0 0 .185-.186V6a.185.185 0 0 0-.185-.186H5.136A.185.185 0 0 0 4.95 6v1.89c0 .103.083.186.186.186Zm2.964 0h2.118a.186.186 0 0 0 .185-.186V6a.186.186 0 0 0-.185-.186H8.1A.185.185 0 0 0 7.914 6v1.89c0 .103.083.186.186.186Zm2.928 0h2.119a.185.185 0 0 0 .185-.186V6a.185.185 0 0 0-.185-.186h-2.119a.186.186 0 0 0-.185.186v1.89c0 .103.083.186.185.186Zm-5.892-2.72h2.118a.185.185 0 0 0 .185-.186V3.28a.186.186 0 0 0-.185-.186H5.136a.186.186 0 0 0-.186.186v1.89c0 .103.083.186.186.186Zm2.964 0h2.118a.186.186 0 0 0 .185-.186V3.28a.186.186 0 0 0-.185-.186H8.1a.186.186 0 0 0-.186.186v1.89c0 .103.083.186.186.186Zm2.928 0h2.119a.185.185 0 0 0 .185-.186V3.28a.186.186 0 0 0-.185-.186h-2.119a.186.186 0 0 0-.185.186v1.89c0 .103.083.186.185.186Zm0-2.72h2.119a.186.186 0 0 0 .185-.186V.56a.185.185 0 0 0-.185-.186h-2.119a.186.186 0 0 0-.185.186v1.89c0 .103.083.186.185.186Zm2.955 5.44h2.118a.185.185 0 0 0 .186-.186V6a.185.185 0 0 0-.186-.186h-2.118a.185.185 0 0 0-.185.186v1.89c0 .103.083.186.185.186Z"></path>
			</svg>
		</div>
		<h1 class="title">Docker Hub 镜像搜索</h1>
		<p class="subtitle">快速查找、下载和部署 Docker 容器镜像</p>
		<div class="search-container">
			<input type="text" id="search-input" placeholder="输入关键词搜索镜像，如: nginx, mysql, redis...">
			<button id="search-button" title="搜索">
				<svg width="20" height="20" fill="none" stroke="currentColor" stroke-width="2" viewBox="0 0 24 24">
					<path d="M13 5l7 7-7 7M5 5l7 7-7 7" stroke-linecap="round" stroke-linejoin="round"></path>
				</svg>
			</button>
		</div>
		<p class="tips">Docker Registry Proxy — 自建镜像代理服务</p>
	</div>
	<script>
	function performSearch() {
		const q = document.getElementById('search-input').value;
		if (q) window.location.href = '/search?q=' + encodeURIComponent(q);
	}
	document.getElementById('search-button').addEventListener('click', performSearch);
	document.getElementById('search-input').addEventListener('keypress', function(e) {
		if (e.key === 'Enter') performSearch();
	});
	window.addEventListener('load', function() { document.getElementById('search-input').focus(); });
	</script>
</body>
</html>`)
}
