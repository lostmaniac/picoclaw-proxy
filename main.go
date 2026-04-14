package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const version = "1.0.0"

var (
	activeConns int64
	totalConns  int64
)

func main() {
	cdpAddr := flag.String("cdp", "127.0.0.1:9222", "CDP 后端地址 (host:port)")
	proxyPort := flag.Int("port", 9222, "代理监听端口")
	allowStr := flag.String("allow", "10.88.7.0/24", "允许连接的网段 (逗号分隔, 例: 10.88.7.0/24,192.168.1.0/24)")
	flag.Parse()

	allowedNets := parseAllowedNets(*allowStr)
	printBanner()

	// 注册全局信号捕获（ waitForCDP 阶段也能优雅退出）
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\n\n  收到退出信号，正在关闭...")
		os.Exit(0)
	}()

	// 1. 检查 CDP 服务
	fmt.Println("[*] 正在检查 CDP 服务...")
	waitForCDP(*cdpAddr)

	// 2. 显示浏览器信息
	showBrowserInfo(*cdpAddr)

	// 3. 获取外部 IP
	extIPs := getExternalIPs()
	if len(extIPs) == 0 {
		log.Fatal("[✗] 未检测到外部网络接口")
	}
	fmt.Printf("[✓] 检测到 %d 个外部网络接口\n", len(extIPs))

	// 4. 启动代理
	listeners, actualPort := startProxy(*cdpAddr, *proxyPort, extIPs, allowedNets)
	if len(listeners) == 0 {
		log.Fatal("[✗] 无法启动代理")
	}

	// 5. 显示连接信息与 Claude 提示词
	showConnectionInfo(extIPs, actualPort, *allowStr)

	// 6. 自检：通过外部 IP 访问代理
	selfTest(extIPs, actualPort)

	// 7. 等待退出
	waitShutdown(listeners)
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
// CDP 检查与等待
// ---------------------------------------------------------------------------

func waitForCDP(addr string) {
	first := true
	for {
		if checkCDP(addr) {
			fmt.Printf("[✓] CDP 已就绪: %s\n", addr)
			return
		}
		if first {
			fmt.Println("[!] 未检测到 CDP 服务")
			showCDPInstructions()
			fmt.Println("[*] 等待 CDP 启动... (每 3 秒重试, Ctrl+C 退出)")
			first = false
		}
		time.Sleep(3 * time.Second)
	}
}

func checkCDP(addr string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "http://"+addr+"/json/version", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

func showBrowserInfo(addr string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+addr+"/json/version", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	var info map[string]interface{}
	if json.NewDecoder(resp.Body).Decode(&info) != nil {
		return
	}
	if v, ok := info["Browser"].(string); ok {
		fmt.Printf("[✓] 浏览器: %s\n", v)
	}
}

// ---------------------------------------------------------------------------
// CDP 启用说明（跨平台）
// ---------------------------------------------------------------------------

func showCDPInstructions() {
	fmt.Println()
	fmt.Println("  ── 请启用浏览器远程调试 ──────────────────────")
	fmt.Println()

	switch runtime.GOOS {
	case "windows":
		fmt.Println("  Chrome (CMD):")
		fmt.Println(`    start "" "C:\Program Files\Google\Chrome\Application\chrome.exe" --remote-debugging-port=9222`)
		fmt.Println()
		fmt.Println("  Chrome (PowerShell):")
		fmt.Println(`    Start-Process "C:\Program Files\Google\Chrome\Application\chrome.exe" -ArgumentList "--remote-debugging-port=9222"`)
		fmt.Println()
		fmt.Println("  Edge (CMD):")
		fmt.Println(`    start "" "C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe" --remote-debugging-port=9222`)
		fmt.Println()
		fmt.Println("  Edge (PowerShell):")
		fmt.Println(`    Start-Process "C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe" -ArgumentList "--remote-debugging-port=9222"`)
	case "darwin":
		fmt.Println("  Chrome:")
		fmt.Println(`    /Applications/Google\ Chrome.app/Contents/MacOS/Google\ Chrome --remote-debugging-port=9222`)
		fmt.Println()
		fmt.Println("  Edge:")
		fmt.Println(`    /Applications/Microsoft\ Edge.app/Contents/MacOS/Microsoft\ Edge --remote-debugging-port=9222`)
	default: // linux
		fmt.Println("  Chrome:")
		fmt.Println("    google-chrome --remote-debugging-port=9222")
		fmt.Println()
		fmt.Println("  Chromium:")
		fmt.Println("    chromium --remote-debugging-port=9222")
		fmt.Println()
		fmt.Println("  Edge:")
		fmt.Println("    microsoft-edge --remote-debugging-port=9222")
	}

	fmt.Println()
	fmt.Println("  或在浏览器地址栏输入:")
	fmt.Println("    chrome://inspect/#remote-debugging")
	fmt.Println("    edge://inspect/#remote-debugging")
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

	// 1) 优先绑定到每个外部 IP（避免与 127.0.0.1:9222 冲突）
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

	// 2) 回退: 尝试 0.0.0.0:port 或 0.0.0.0:port+1
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

	// IP 白名单
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

	// 连接 CDP 后端
	backend, err := net.DialTimeout("tcp", cdpAddr, 5*time.Second)
	if err != nil {
		log.Printf("[!] CDP 后端连接失败: %v", err)
		return
	}
	defer backend.Close()

	// 双向转发
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
		testURL := fmt.Sprintf("http://%s:%d/json/version", ip, port)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		req, _ := http.NewRequestWithContext(ctx, "GET", testURL, nil)
		resp, err := http.DefaultClient.Do(req)
		cancel()
		if err != nil {
			fmt.Printf("  [✗] %s - 代理自检失败: %v\n", ip, err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == 200 {
			fmt.Printf("  [✓] %s:%d - 代理工作正常\n", ip, port)
		} else {
			fmt.Printf("  [?] %s:%d - 状态码 %d\n", ip, port, resp.StatusCode)
		}
	}
	fmt.Println()
}

// ---------------------------------------------------------------------------
// 连接信息展示
// ---------------------------------------------------------------------------

func showConnectionInfo(ips []net.IP, port int, allowStr string) {
	fmt.Println()
	fmt.Println("  ═════════════════════════════════════════════════")
	fmt.Println("    CDP 代理已启动")
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

	fmt.Println("  ── CDP 连接地址 ─────────────────────────────────")
	fmt.Println()
	for _, ip := range ips {
		fmt.Printf("    版本: http://%s:%d/json/version\n", ip, port)
		fmt.Printf("    页面: http://%s:%d/json\n", ip, port)
	}
	fmt.Println()

	fmt.Println("  ── Claude 提示词 (复制下方内容) ──────────────────")
	fmt.Println()

	ip := ips[0]
	fmt.Printf(`  请使用 CDP (Chrome DevTools Protocol) 模式连接我的浏览器。

  连接步骤:
  1. 访问 http://%s:%d/json/version 确认 CDP 连接正常
  2. 访问 http://%s:%d/json 获取可调试的页面列表
  3. 注意: 返回的 webSocketDebuggerUrl 中地址为 127.0.0.1:9222，
     需要替换为 %s:%d
  4. 使用替换后的 WebSocket 地址连接到目标页面
  5. 连接成功后，执行 document.title 测试连接是否正常

  CDP 代理地址: %s:%d
`, ip, port, ip, port, ip, port, ip, port)

	fmt.Println("  ═════════════════════════════════════════════════")
	fmt.Println()
	fmt.Println("  等待连接中... (Ctrl+C 退出)")
	fmt.Println()
}

// ---------------------------------------------------------------------------
// 优雅退出
// ---------------------------------------------------------------------------

func waitShutdown(listeners []net.Listener) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	s := <-sigCh
	fmt.Printf("\n\n  收到信号 %v，正在关闭...\n", s)
	for _, ln := range listeners {
		ln.Close()
	}
	fmt.Println("  [✓] 已停止")
}
