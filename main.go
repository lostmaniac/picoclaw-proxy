package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const version = "1.5.0"

var (
	activeConns int64
	totalConns  int64
)

func main() {
	cdpAddr := flag.String("cdp", "127.0.0.1:9222", "CDP 后端地址 (host:port)")
	proxyPort := flag.Int("port", 9222, "代理监听端口")
	allowStr := flag.String("allow", "10.88.7.0/24", "允许连接的网段 (逗号分隔)")
	flag.Parse()

	allowedNets := parseAllowedNets(*allowStr)
	printBanner()

	// 0. 自动启动浏览器
	var chromeProc *os.Process
	fmt.Println("[*] 正在搜索浏览器...")
	chromeProc = launchChrome(*cdpAddr)
	if chromeProc == nil {
		showCDPInstructions()
	}

	// 1. 等待浏览器调试端口
	fmt.Println("[*] 正在检查调试端口...")
	waitForPort(*cdpAddr)

	// 2. 获取外部 IP
	extIPs := getExternalIPs()
	if len(extIPs) == 0 {
		log.Fatal("[✗] 未检测到外部网络接口")
	}
	fmt.Printf("[✓] 检测到 %d 个外部网络接口\n", len(extIPs))

	// 3. 启动代理
	listeners, actualPort := startProxy(*cdpAddr, *proxyPort, extIPs, allowedNets)
	if len(listeners) == 0 {
		log.Fatal("[✗] 无法启动代理")
	}

	// 4. 显示连接信息与 PicoClaw 提示词
	showConnectionInfo(extIPs, actualPort, *allowStr)

	// 5. 自检
	selfTest(extIPs, actualPort)

	// 6. 等待退出
	waitShutdown(listeners, chromeProc)
}

// ---------------------------------------------------------------------------
// CIDR 解析
// ---------------------------------------------------------------------------

func parseAllowedNets(s string) []*net.IPNet {
	var nets []*net.IPNet
	for _, cidr := range strings.Split(s, ",") {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			continue
		}
		if !strings.Contains(cidr, "/") {
			cidr += "/32"
		}
		_, ipNet, err := net.ParseCIDR(cidr)
		if err != nil {
			log.Fatalf("无效的 CIDR %q: %v", cidr, err)
		}
		nets = append(nets, ipNet)
	}
	if len(nets) == 0 {
		log.Fatal("至少需要一个允许的网段 (-allow)")
	}
	return nets
}

// ---------------------------------------------------------------------------
// Banner
// ---------------------------------------------------------------------------

func printBanner() {
	fmt.Println()
	fmt.Println("  ┌──────────────────────────────────────────┐")
	fmt.Printf("  │  PicoClaw Browser CDP Proxy  v%-10s  │\n", version)
	fmt.Println("  │  Chrome DevTools Protocol 转发服务        │")
	fmt.Println("  └──────────────────────────────────────────┘")
	fmt.Println()
	fmt.Printf("  平台: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Println()
}

// ---------------------------------------------------------------------------
// Chrome 自动启动
// ---------------------------------------------------------------------------

func findChrome() string {
	var candidates []string

	switch runtime.GOOS {
	case "windows":
		localAppData := os.Getenv("LOCALAPPDATA")
		candidates = []string{
			`C:\Program Files\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		}
		if localAppData != "" {
			candidates = append(candidates,
				localAppData+`\Google\Chrome\Application\chrome.exe`,
			)
		}
		candidates = append(candidates,
			`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
			`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		)
		if localAppData != "" {
			candidates = append(candidates,
				localAppData+`\Microsoft\Edge\Application\msedge.exe`,
			)
		}

	case "darwin":
		home, _ := os.UserHomeDir()
		candidates = []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		}
		if home != "" {
			candidates = append(candidates,
				filepath.Join(home, "Applications", "Google Chrome.app", "Contents", "MacOS", "Google Chrome"),
			)
		}
		candidates = append(candidates,
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
		)

	default:
		candidates = []string{
			"google-chrome",
			"google-chrome-stable",
			"chromium",
			"chromium-browser",
			"/usr/bin/google-chrome",
			"/usr/bin/google-chrome-stable",
			"/usr/bin/chromium",
			"/usr/bin/chromium-browser",
			"/snap/bin/chromium",
			"/usr/local/bin/chrome",
			"/usr/local/bin/chromium",
		}
	}

	for _, c := range candidates {
		if runtime.GOOS == "linux" && !strings.Contains(c, "/") {
			if p, err := exec.LookPath(c); err == nil {
				return p
			}
		} else {
			if _, err := os.Stat(c); err == nil {
				return c
			}
		}
	}
	return ""
}

func getUserDataDir(port string) string {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".picoclaw-browser-proxy", "chrome_mcp_data_"+port)
	os.MkdirAll(dir, 0700)
	return dir
}

func extractPort(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "9222"
	}
	return port
}

func launchChrome(cdpAddr string) *os.Process {
	chromePath := findChrome()
	if chromePath == "" {
		fmt.Println("[✗] 未找到 Chrome 或 Edge 浏览器")
		return nil
	}

	port := extractPort(cdpAddr)
	userDataDir := getUserDataDir(port)

	args := []string{
		"--remote-debugging-port=" + port,
		"--user-data-dir=" + userDataDir,
		"--no-first-run",
	}

	cmd := exec.Command(chromePath, args...)
	if err := cmd.Start(); err != nil {
		fmt.Printf("[✗] 启动浏览器失败: %v\n", err)
		return nil
	}

	fmt.Printf("[✓] 已启动浏览器 (PID: %d): %s\n", cmd.Process.Pid, chromePath)
	return cmd.Process
}

func killChrome(proc *os.Process) {
	if proc == nil {
		return
	}
	if runtime.GOOS == "windows" {
		exec.Command("taskkill", "/T", "/F", "/PID", fmt.Sprintf("%d", proc.Pid)).Run()
	} else {
		proc.Signal(syscall.SIGTERM)
		proc.Wait()
	}
	fmt.Println("[✓] 浏览器进程已终止")
}

// ---------------------------------------------------------------------------
// 端口检查
// ---------------------------------------------------------------------------

func waitForPort(addr string) {
	first := true
	for {
		if checkPortOpen(addr) {
			fmt.Printf("[✓] 调试端口已就绪: %s\n", addr)
			return
		}
		if first {
			fmt.Println("[!] 未检测到调试端口")
			showCDPInstructions()
			fmt.Println("[*] 等待端口开启... (每 3 秒重试, Ctrl+C 退出)")
			first = false
		}
		time.Sleep(3 * time.Second)
	}
}

func checkPortOpen(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// ---------------------------------------------------------------------------
// CDP 启用说明（跨平台）
// ---------------------------------------------------------------------------

func showCDPInstructions() {
	fmt.Println()
	fmt.Println("  ── 请启用浏览器远程调试 ──────────────────────")
	fmt.Println()
	fmt.Println("  方式一: 浏览器设置")
	fmt.Println("    Chrome: 设置 → 远程调试 → 开启")
	fmt.Println("    Edge:   设置 → 远程调试 → 开启")
	fmt.Println()
	fmt.Println("  方式二: 命令行启动")

	switch runtime.GOOS {
	case "windows":
		fmt.Println(`    "C:\Program Files\Google\Chrome\Application\chrome.exe" --remote-debugging-port=9222 --user-data-dir="%USERPROFILE%\.picoclaw-browser-proxy\chrome_mcp_data"`)
	case "darwin":
		fmt.Println(`    /Applications/Google\ Chrome.app/Contents/MacOS/Google\ Chrome --remote-debugging-port=9222 --user-data-dir=$HOME/.picoclaw-browser-proxy/chrome_mcp_data`)
	default:
		fmt.Println("    google-chrome --remote-debugging-port=9222 --user-data-dir=$HOME/.picoclaw-browser-proxy/chrome_mcp_data")
	}

	fmt.Println()
}

// ---------------------------------------------------------------------------
// 网络工具
// ---------------------------------------------------------------------------

func getExternalIPs() []net.IP {
	var ips []net.IP
	ifaces, err := net.Interfaces()
	if err != nil {
		return ips
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip != nil && !ip.IsLoopback() && ip.To4() != nil {
				ips = append(ips, ip)
			}
		}
	}
	return ips
}

// ---------------------------------------------------------------------------
// 代理启动
// ---------------------------------------------------------------------------

func startProxy(cdpAddr string, port int, extIPs []net.IP, allowed []*net.IPNet) ([]net.Listener, int) {
	var listeners []net.Listener
	actualPort := port

	for _, ip := range extIPs {
		addr := fmt.Sprintf("%s:%d", ip.String(), port)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			log.Printf("[!] 监听 %s 失败: %v", addr, err)
			continue
		}
		listeners = append(listeners, ln)
		go acceptLoop(ln, cdpAddr, allowed)
		fmt.Printf("[✓] 监听: %s\n", addr)
	}

	if len(listeners) == 0 {
		for offset := 0; offset <= 1; offset++ {
			p := port + offset
			addr := fmt.Sprintf("0.0.0.0:%d", p)
			ln, err := net.Listen("tcp", addr)
			if err == nil {
				listeners = append(listeners, ln)
				go acceptLoop(ln, cdpAddr, allowed)
				actualPort = p
				fmt.Printf("[✓] 监听: %s\n", addr)
				if offset > 0 {
					log.Printf("[!] 端口 %d 被占用，已改用端口 %d", port, p)
				}
				break
			}
			log.Printf("[!] 监听 %s 失败: %v", addr, err)
		}
	}

	return listeners, actualPort
}

// ---------------------------------------------------------------------------
// 连接循环
// ---------------------------------------------------------------------------

func acceptLoop(ln net.Listener, cdpAddr string, allowed []*net.IPNet) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if !strings.Contains(err.Error(), "closed network") {
				log.Printf("[!] Accept 错误: %v", err)
			}
			return
		}
		go handleConnection(conn, cdpAddr, allowed)
	}
}

func handleConnection(conn net.Conn, cdpAddr string, allowed []*net.IPNet) {
	defer conn.Close()

	remoteIP := conn.RemoteAddr().(*net.TCPAddr).IP

	if !isAllowedIP(remoteIP, allowed) {
		log.Printf("[✗] 拒绝: %s", remoteIP)
		return
	}

	atomic.AddInt64(&activeConns, 1)
	total := atomic.AddInt64(&totalConns, 1)
	log.Printf("[→] 连接 #%d: %s (活跃: %d)", total, remoteIP, atomic.LoadInt64(&activeConns))

	defer func() {
		atomic.AddInt64(&activeConns, -1)
		log.Printf("[←] 断开: %s (活跃: %d, 总计: %d)", remoteIP, atomic.LoadInt64(&activeConns), atomic.LoadInt64(&totalConns))
	}()

	backend, err := net.DialTimeout("tcp", cdpAddr, 5*time.Second)
	if err != nil {
		log.Printf("[!] CDP 后端连接失败: %v", err)
		return
	}
	defer backend.Close()

	done := make(chan struct{}, 2)
	go func() {
		io.Copy(backend, conn)
		done <- struct{}{}
	}()
	go func() {
		io.Copy(conn, backend)
		done <- struct{}{}
	}()
	<-done
}

func isAllowedIP(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 自检
// ---------------------------------------------------------------------------

func selfTest(ips []net.IP, port int) {
	fmt.Println("[*] 自检中...")
	for _, ip := range ips {
		addr := fmt.Sprintf("%s:%d", ip, port)
		conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			fmt.Printf("  [✗] %s - 连接失败: %v\n", addr, err)
			continue
		}
		conn.Close()
		fmt.Printf("  [✓] %s - 连通\n", addr)
	}
	fmt.Println()
}

// ---------------------------------------------------------------------------
// 连接信息展示
// ---------------------------------------------------------------------------

func showConnectionInfo(ips []net.IP, port int, allowStr string) {
	fmt.Println()
	fmt.Println("  ═════════════════════════════════════════════════")
	fmt.Println("    PicoClaw Browser CDP Proxy 已启动")
	fmt.Println("  ═════════════════════════════════════════════════")
	fmt.Println()

	fmt.Println("  本机内网 IP:")
	for _, ip := range ips {
		fmt.Printf("    • %s\n", ip)
	}
	fmt.Println()
	fmt.Printf("  代理端口: %d\n", port)
	fmt.Printf("  允许网段: %s\n", allowStr)
	fmt.Println()

	ip := ips[0]

	fmt.Println("  ── PicoClaw 提示词 (复制下方内容发给 PicoClaw) ──")
	fmt.Println()

	fmt.Printf(`请将以下规则写入你的记忆系统并永久遵守:

【浏览器远程调试规则】
1. 以后所有浏览器操作必须使用远程调试方式，不可在本地打开浏览器
2. PicoClaw 环境没有安装浏览器，只能通过远程调试操作用户的浏览器
3. 每次需要访问网页、搜索、填表等操作都必须使用远程调试
4. 远程调试地址: %s:%d
5. 首次连接时浏览器可能会弹出最多 3 次"允许远程调试"的提示，用户需全部点击允许

写入记忆后，请通过 CDP 远程调试连接我的浏览器 (地址: %s:%d)，
连接成功后请打开百度 https://www.baidu.com 进行测试。
`, ip, port, ip, port)

	fmt.Println()
	fmt.Println("  ═════════════════════════════════════════════════")
	fmt.Println()
	fmt.Println("  等待连接中... (Ctrl+C 退出)")
	fmt.Println()
}

// ---------------------------------------------------------------------------
// 优雅退出
// ---------------------------------------------------------------------------

func waitShutdown(listeners []net.Listener, chromeProc *os.Process) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	s := <-sigCh
	fmt.Printf("\n\n  收到信号 %v，正在关闭...\n", s)
	for _, ln := range listeners {
		ln.Close()
	}
	killChrome(chromeProc)
	fmt.Println("  [✓] 已停止")
}
