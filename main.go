package main

import (
	"bufio"
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
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"
)

const version = "2.0.0"

var (
	activeConns int64
	totalConns  int64
)

type Config struct {
	Host       string `yaml:"host"`
	Port       int    `yaml:"port"`
	User       string `yaml:"user"`
	Password   string `yaml:"password"`
	PrivateKey string `yaml:"private_key"`
	LocalAddr  string `yaml:"local_addr"`
	LocalPort  int    `yaml:"local_port"`
	RemoteAddr string `yaml:"remote_addr"`
	RemotePort int    `yaml:"remote_port"`
}

func (c *Config) applyDefaults() {
	if c.Port == 0 {
		c.Port = 22
	}
	if c.User == "" {
		c.User = "root"
	}
	if c.LocalAddr == "" {
		c.LocalAddr = "127.0.0.1"
	}
	if c.LocalPort == 0 {
		c.LocalPort = 9222
	}
	if c.RemoteAddr == "" {
		c.RemoteAddr = "127.0.0.1"
	}
	if c.RemotePort == 0 {
		c.RemotePort = 9222
	}
}

func main() {
	configPath := flag.String("config", "", "配置文件路径")
	flag.Parse()

	printBanner()

	cfgPath, err := findConfig(*configPath)
	if err != nil {
		fatal("%v", err)
	}
	fmt.Printf("[✓] 配置文件: %s\n", cfgPath)

	cfg, err := loadConfig(cfgPath)
	if err != nil {
		fatal("%v", err)
	}

	fmt.Printf("[*] 目标: %s@%s:%d\n", cfg.User, cfg.Host, cfg.Port)
	fmt.Printf("[*] 转发: %s:%d → %s:%d\n", cfg.LocalAddr, cfg.LocalPort, cfg.RemoteAddr, cfg.RemotePort)

	localListen := fmt.Sprintf("%s:%d", cfg.LocalAddr, cfg.LocalPort)
	listener, err := net.Listen("tcp", localListen)
	if err != nil {
		procInfo := findPortProcess(cfg.LocalPort)
		if procInfo != "" {
			fatal("端口 %d 已被占用: %s", cfg.LocalPort, procInfo)
		}
		fatal("本地监听失败: %v", err)
	}
	defer listener.Close()
	fmt.Printf("[✓] 本地监听: %s\n", localListen)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	var (
		clientMu      sync.RWMutex
		currentClient *ssh.Client
		clientDead    atomic.Bool
	)

	// 单个 acceptLoop，整个生命周期不退出
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			if clientDead.Load() {
				conn.Close()
				continue
			}
			clientMu.RLock()
			c := currentClient
			clientMu.RUnlock()
			if c == nil {
				conn.Close()
				continue
			}
			go handleForward(conn, c, cfg)
		}
	}()

	firstConnect := true
	for {
		client, err := dialSSH(cfg)
		if err != nil {
			log.Printf("[✗] SSH 连接失败: %v", err)
			fmt.Println("[*] 5 秒后重试...")
			select {
			case <-sigCh:
				fmt.Printf("\n  [✓] 已停止\n")
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}

		clientDead.Store(false)
		clientMu.Lock()
		currentClient = client
		clientMu.Unlock()

		if firstConnect {
			showReady(cfg)
			firstConnect = false
		} else {
			log.Println("[✓] SSH 重连成功")
		}

		go checkRemotePort(client, cfg)

		clientDone := make(chan struct{})
		go func() {
			keepaliveLoop(client)
			close(clientDone)
		}()

		select {
		case <-sigCh:
			client.Close()
			fmt.Printf("\n\n  正在关闭...\n  [✓] 已停止\n")
			return
		case <-clientDone:
			clientDead.Store(true)
			clientMu.Lock()
			currentClient = nil
			clientMu.Unlock()
			client.Close()
			log.Println("[!] SSH 连接断开，正在重连...")
		}
	}
}

func dialSSH(cfg *Config) (*ssh.Client, error) {
	sshConfig := &ssh.ClientConfig{
		User:            cfg.User,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	if cfg.PrivateKey != "" {
		signer, err := ssh.ParsePrivateKey([]byte(cfg.PrivateKey))
		if err != nil {
			return nil, fmt.Errorf("解析私钥失败: %v", err)
		}
		sshConfig.Auth = append(sshConfig.Auth, ssh.PublicKeys(signer))
	}
	if cfg.Password != "" {
		sshConfig.Auth = append(sshConfig.Auth, ssh.Password(cfg.Password))
	}
	if len(sshConfig.Auth) == 0 {
		return nil, fmt.Errorf("未指定 private_key 或 password")
	}

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	return ssh.Dial("tcp", addr, sshConfig)
}

func keepaliveLoop(client *ssh.Client) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
		if err != nil {
			return
		}
	}
}

func checkRemotePort(client *ssh.Client, cfg *Config) {
	remoteAddr := fmt.Sprintf("%s:%d", cfg.RemoteAddr, cfg.RemotePort)
	conn, err := client.Dial("tcp", remoteAddr)
	if err != nil {
		fmt.Printf("[!] 远程 %s 未就绪，请确认远程浏览器已开启调试端口\n", remoteAddr)
		fmt.Println("[*] 连接进来时会自动重试转发")
		return
	}
	conn.Close()
	fmt.Printf("[✓] 远程 %s 已就绪\n", remoteAddr)
}

func handleForward(localConn net.Conn, client *ssh.Client, cfg *Config) {
	defer localConn.Close()

	remoteAddr := fmt.Sprintf("%s:%d", cfg.RemoteAddr, cfg.RemotePort)
	sshConn, err := client.Dial("tcp", remoteAddr)
	if err != nil {
		if strings.Contains(err.Error(), "Connection refused") {
			log.Printf("[!] 远程浏览器未启动 (远程 %s:%d 未监听)", cfg.RemoteAddr, cfg.RemotePort)
		} else {
			log.Printf("[!] 远程连接失败: %v", err)
		}
		return
	}
	defer sshConn.Close()

	n := atomic.AddInt64(&totalConns, 1)
	active := atomic.AddInt64(&activeConns, 1)
	log.Printf("[→] 连接 #%d (活跃: %d)", n, active)

	defer func() {
		atomic.AddInt64(&activeConns, -1)
		log.Printf("[←] 断开 #%d (活跃: %d)", n, atomic.LoadInt64(&activeConns))
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		io.Copy(sshConn, localConn)
		sshConn.Close()
	}()
	go func() {
		defer wg.Done()
		io.Copy(localConn, sshConn)
		localConn.Close()
	}()
	wg.Wait()
}

func findConfig(custom string) (string, error) {
	if custom != "" {
		if _, err := os.Stat(custom); err != nil {
			return "", fmt.Errorf("配置文件不存在: %s", custom)
		}
		return custom, nil
	}

	for _, name := range []string{"picoclaw_proxy_config.yaml", "config.yaml"} {
		if _, err := os.Stat(name); err == nil {
			return name, nil
		}
	}

	matches, _ := filepath.Glob("*.yaml")
	if len(matches) > 0 {
		sort.Strings(matches)
		return matches[0], nil
	}

	return "", fmt.Errorf("未找到配置文件，请放置 picoclaw_proxy_config.yaml 或使用 -config 指定路径")
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取配置失败: %v", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析 YAML 失败: %v", err)
	}
	cfg.applyDefaults()
	if cfg.Host == "" {
		return nil, fmt.Errorf("配置缺少 host 字段")
	}
	return &cfg, nil
}

func fatal(format string, args ...interface{}) {
	fmt.Printf("[✗] "+format+"\n", args...)
	fmt.Println("\n按 Enter 键退出...")
	bufio.NewReader(os.Stdin).ReadString('\n')
	os.Exit(1)
}

func printBanner() {
	fmt.Println()
	fmt.Println("  ┌──────────────────────────────────────────┐")
	fmt.Printf("  │  PicoClaw SSH Tunnel Proxy   v%-10s  │\n", version)
	fmt.Println("  │  Chrome DevTools Protocol 端口转发        │")
	fmt.Println("  └──────────────────────────────────────────┘")
	fmt.Println()
	fmt.Printf("  平台: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Println()
}

func findPortProcess(port int) string {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("cmd", "/c",
			fmt.Sprintf("netstat -aon | findstr \":%d \" | findstr LISTENING", port))
	case "darwin":
		cmd = exec.Command("lsof", "-i", fmt.Sprintf(":%d", port), "-sTCP:LISTEN", "-t")
	default:
		cmd = exec.Command("ss", "-tlnp", fmt.Sprintf("sport = :%d", port))
	}
	out, err := cmd.CombinedOutput()
	if err != nil || len(out) == 0 {
		return ""
	}

	switch runtime.GOOS {
	case "windows":
		fields := strings.Fields(strings.TrimSpace(string(out)))
		if len(fields) >= 5 {
			pid := fields[len(fields)-1]
			nameOut, _ := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %s", pid), "/FO", "CSV", "/NH").Output()
			parts := strings.Split(string(nameOut), ",")
			if len(parts) >= 1 {
				name := strings.Trim(parts[0], "\"")
				if name != "" {
					return fmt.Sprintf("%s (PID %s)", name, pid)
				}
			}
			return "PID " + pid
		}

	case "darwin":
		pid := strings.TrimSpace(string(out))
		pid = strings.Split(pid, "\n")[0]
		pid = strings.TrimSpace(pid)
		nameOut, _ := exec.Command("ps", "-p", pid, "-o", "comm=").Output()
		name := strings.TrimSpace(string(nameOut))
		if name != "" {
			return fmt.Sprintf("%s (PID %s)", name, pid)
		}
		return "PID " + pid

	default: // linux
		for _, line := range strings.Split(string(out), "\n") {
			if idx := strings.Index(line, "users:((\""); idx != -1 {
				part := line[idx+9:]
				if end := strings.Index(part, "\""); end > 0 {
					name := part[:end]
					if pidx := strings.Index(part, "pid="); pidx > 0 {
						rest := part[pidx+4:]
						pidEnd := strings.IndexAny(rest, ",)")
						if pidEnd > 0 {
							return fmt.Sprintf("%s (PID %s)", name, rest[:pidEnd])
						}
					}
					return name
				}
			}
		}
	}
	return ""
}

func showReady(cfg *Config) {
	fmt.Println()
	fmt.Println("  ═════════════════════════════════════════════════")
	fmt.Println("    隧道建立成功，可以远程控制浏览器了")
	fmt.Println("  ═════════════════════════════════════════════════")
	fmt.Println()
	fmt.Printf("  本地地址:   %s:%d\n", cfg.LocalAddr, cfg.LocalPort)
	fmt.Printf("  远程地址:   %s:%d\n", cfg.RemoteAddr, cfg.RemotePort)
	fmt.Printf("  SSH 服务器: %s@%s:%d\n", cfg.User, cfg.Host, cfg.Port)
	fmt.Println()
	fmt.Println("  ── MCP 服务器配置 ──")
	fmt.Println()
	fmt.Println("  请将 PicoClaw 的 MCP 服务器地址配置为:")
	fmt.Printf("  http://127.0.0.1:%d\n", cfg.LocalPort)
	fmt.Println()
	fmt.Println("  ═════════════════════════════════════════════════")
	fmt.Println()
	fmt.Println("  等待连接中... (Ctrl+C 退出)")
	fmt.Println()
}
