package main

import (
	"bufio"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"mote/internal/agent"
	"mote/internal/shared"
)

const banner = `
╔════════════════════════════════════════╗
║         📡 被控管理面板 v%s          
╚════════════════════════════════════════╝
`

func main() {
	if len(os.Args) < 2 {
		showMenu()
		return
	}

	switch os.Args[1] {
	case "run":
		// 守护进程入口(systemd 调用)
		cmdRun()
	case "start":
		cmdSystemctl("start", "bk")
	case "stop":
		cmdSystemctl("stop", "bk")
	case "restart":
		cmdSystemctl("restart", "bk")
	case "status":
		cmdStatus()
	case "reconfig":
		cmdReconfig()
	case "uninstall":
		cmdUninstall()
	case "version":
		fmt.Println("bk version", shared.Version)
	case "install":
		cmdInstall()
	case "-h", "--help", "help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`Usage: bk [command]

Commands:
  (no args)    Open interactive menu
  run          Run as daemon (used by systemd)
  start        Start the systemd service
  stop         Stop the service
  restart      Restart the service
  status       Show connection status
  reconfig     Reconfigure server URL and token
  install      Install as systemd service
  uninstall    Uninstall (stop service, remove files)
  version      Show version`)
}

// cmdRun 是守护模式入口
func cmdRun() {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	configPath := fs.String("c", agent.DefaultConfigPath, "config file path")
	// 允许通过参数覆盖,方便首次启动测试
	server := fs.String("s", "", "server URL (overrides config)")
	token := fs.String("t", "", "token (overrides config)")
	adKey := fs.String("ad", "", "auto-discovery key (overrides config)")
	fs.Parse(os.Args[2:])

	var cfg *agent.Config
	if c, err := agent.LoadConfig(*configPath); err == nil {
		cfg = c
	} else {
		// 配置文件不存在,用参数构造一个临时配置
		cfg = agent.NewConfig(*configPath, *server, *token, *adKey)
	}
	if *server != "" {
		cfg.Server = *server
	}
	if *token != "" {
		cfg.Token = *token
	}
	if *adKey != "" {
		cfg.AutoDiscovery = *adKey
	}

	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(1)
	}

	if err := agent.Run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "agent exited:", err)
		os.Exit(1)
	}
}

func cmdSystemctl(action, unit string) {
	cmd := exec.Command("systemctl", action, unit)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run()
}

func cmdStatus() {
	cfg, err := agent.LoadConfig(agent.DefaultConfigPath)
	if err != nil {
		fmt.Println("❌ 未找到配置,Agent 可能未安装")
		return
	}
	fmt.Println("配置文件 :", agent.DefaultConfigPath)
	fmt.Println("主控地址 :", cfg.Server)
	fmt.Println("采集间隔 :", cfg.Interval, "秒")
	fmt.Println("心跳间隔 :", cfg.Heartbeat, "秒")

	// 服务状态
	out, _ := exec.Command("systemctl", "is-active", "bk").Output()
	state := strings.TrimSpace(string(out))
	switch state {
	case "active":
		fmt.Println("服务状态 : ● 运行中")
	case "inactive", "failed":
		fmt.Println("服务状态 : ○ 未运行")
	default:
		fmt.Println("服务状态 :", state)
	}

	// 探测主控连通性
	url := strings.Replace(cfg.Server, "wss://", "https://", 1)
	url = strings.Replace(url, "ws://", "http://", 1)
	client := http.Client{Timeout: 5 * time.Second}
	if _, err := client.Get(url); err == nil {
		fmt.Println("主控连通 : ✓")
	} else {
		fmt.Println("主控连通 : ✗", err)
	}
}

func cmdReconfig() {
	checkRoot()
	cfg, err := agent.LoadConfig(agent.DefaultConfigPath)
	if err != nil {
		cfg = agent.NewConfig(agent.DefaultConfigPath, "", "", "")
	}
	r := bufio.NewReader(os.Stdin)

	// 保存旧值用于回滚
	oldServer := cfg.Server
	oldToken := cfg.Token

	fmt.Printf("主控地址 [%s]: ", cfg.Server)
	if line, _ := r.ReadString('\n'); strings.TrimSpace(line) != "" {
		cfg.Server = strings.TrimSpace(line)
	}

	fmt.Printf("Token [%s]: ", maskToken(cfg.Token))
	if line, _ := r.ReadString('\n'); strings.TrimSpace(line) != "" {
		cfg.Token = strings.TrimSpace(line)
	}

	if err := cfg.Save(); err != nil {
		fmt.Println("保存失败:", err)
		return
	}
	fmt.Println("✓ 配置已保存,正在重启服务...")
	exec.Command("systemctl", "restart", "bk").Run()

	// 60 秒内每 5 秒探测一次主控连通性
	probeURL := strings.Replace(cfg.Server, "wss://", "https://", 1)
	probeURL = strings.Replace(probeURL, "ws://", "http://", 1)
	probeURL = strings.TrimRight(probeURL, "/") + "/api/config"

	client := http.Client{Timeout: 4 * time.Second}
	deadline := time.Now().Add(60 * time.Second)
	connected := false
	for time.Now().Before(deadline) {
		time.Sleep(5 * time.Second)
		resp, err := client.Get(probeURL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				connected = true
				break
			}
		}
		fmt.Print(".")
	}
	fmt.Println()

	if connected {
		fmt.Println("✅ 主控连通,配置生效")
	} else {
		fmt.Println("❌ 60 秒内无法连通新主控,正在回滚配置...")
		cfg.Server = oldServer
		cfg.Token = oldToken
		if err := cfg.Save(); err != nil {
			fmt.Println("回滚保存失败:", err)
			return
		}
		exec.Command("systemctl", "restart", "bk").Run()
		fmt.Println("✓ 已回滚到旧配置并重启服务")
	}
}

func cmdInstall() {
	checkRoot()
	fmt.Println("此命令通常由 install-bk.sh 调用,请使用安装脚本完成完整安装。")
	fmt.Println("如需手动配置,使用: bk reconfig")
}

func cmdUninstall() {
	checkRoot()
	fmt.Println("即将卸载被控,包括:")
	fmt.Println("  · 停止 bk 服务")
	fmt.Println("  · 删除 /etc/bk, /var/log/bk, /opt/bk")
	fmt.Println("  · 删除 systemd 单元文件")
	fmt.Print("继续? [y/N]: ")
	r := bufio.NewReader(os.Stdin)
	line, _ := r.ReadString('\n')
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y") {
		fmt.Println("已取消")
		return
	}
	if err := agent.DoLocalUninstall(); err != nil {
		fmt.Println("卸载失败:", err)
		return
	}
	fmt.Println("✅ 服务已卸载")
	fmt.Println("如需彻底删除二进制,执行: sudo rm /usr/local/bin/bk")
}

func showMenu() {
	for {
		fmt.Printf(banner, shared.Version)

		// 状态概览
		cfg, err := agent.LoadConfig(agent.DefaultConfigPath)
		if err == nil {
			out, _ := exec.Command("systemctl", "is-active", "bk").Output()
			state := strings.TrimSpace(string(out))
			fmt.Println("  状态  :", state)
			fmt.Println("  主控  :", cfg.Server)
		} else {
			fmt.Println("  状态  : 未安装/未配置")
		}

		fmt.Println()
		fmt.Println("  [1] 查看状态")
		fmt.Println("  [2] 启动")
		fmt.Println("  [3] 停止")
		fmt.Println("  [4] 重启")
		fmt.Println("  [5] 重新配置")
		fmt.Println("  [6] 卸载")
		fmt.Println("  [0] 退出")
		fmt.Println()
		fmt.Print("  选择 [0-6]: ")
		r := bufio.NewReader(os.Stdin)
		line, _ := r.ReadString('\n')
		switch strings.TrimSpace(line) {
		case "1":
			cmdStatus()
		case "2":
			cmdSystemctl("start", "bk")
		case "3":
			cmdSystemctl("stop", "bk")
		case "4":
			cmdSystemctl("restart", "bk")
		case "5":
			cmdReconfig()
		case "6":
			cmdUninstall()
			return
		case "0", "":
			return
		}
		fmt.Println("\n按回车继续...")
		bufio.NewReader(os.Stdin).ReadString('\n')
	}
}

func checkRoot() {
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "❌ 此操作需要 root,请使用 sudo")
		os.Exit(1)
	}
}

func maskToken(t string) string {
	if len(t) < 6 {
		return "(未设置)"
	}
	return t[:3] + "***" + t[len(t)-3:]
}
