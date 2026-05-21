package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"mote/internal/server"
	"mote/internal/shared"
)

const banner = `
╔════════════════════════════════════════╗
║         🚀 主控管理面板 v%s          
╚════════════════════════════════════════╝
`

func main() {
	if len(os.Args) < 2 {
		showMenu()
		return
	}
	switch os.Args[1] {
	case "run":
		cmdRun()
	case "start":
		cmdSystemctl("start", "zk")
	case "stop":
		cmdSystemctl("stop", "zk")
	case "restart":
		cmdSystemctl("restart", "zk")
	case "status":
		cmdStatus()
	case "uninstall":
		cmdUninstall()
	case "update":
		cmdUpdate()
	case "version":
		fmt.Println("zk version", shared.Version)
	case "-h", "--help", "help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`Usage: zk [command]

Commands:
  (no args)    Open interactive menu
  run          Run as daemon (used by systemd)
  start        Start the systemd service
  stop         Stop the service
  restart      Restart the service
  status       Show service status and panel URL
  update       Update to the latest release (auto rollback on failure)
  uninstall    Uninstall (stop service, remove files)
  version      Show version`)
}

func cmdRun() {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	configPath := fs.String("c", server.DefaultConfigPath, "config file path")
	fs.Parse(os.Args[2:])

	cfg, err := server.LoadOrCreateConfig(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0755); err != nil {
		log.Fatalf("create data dir: %v", err)
	}

	store, err := server.OpenStore(cfg.DBPath())
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer store.Close()

	hub := server.NewHub(store, cfg)
	selfMon := server.NewSelfMonitor(store, cfg)
	selfMon.Start()
	api := server.NewAPI(store, hub, cfg, selfMon)
	sched := server.NewScheduler(store, hub)
	sched.Start()
	defer sched.Stop()

	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: api.Routes(),
	}

	log.Printf("zk %s listening on %s", shared.Version, cfg.Listen)
	log.Printf("admin user: %s", cfg.AdminUsername)
	log.Printf("admin pass: %s", cfg.AdminPassword)
	if cfg.AutoDiscoveryKey != "" {
		log.Printf("auto-discovery key: %s", cfg.AutoDiscoveryKey)
	}

	// 优雅退出
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	<-sigc
	log.Println("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}

func cmdSystemctl(action, unit string) {
	cmd := exec.Command("systemctl", action, unit)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run()
}

func cmdStatus() {
	cfg, err := server.LoadOrCreateConfig(server.DefaultConfigPath)
	if err != nil {
		fmt.Println("❌ 未找到配置")
		return
	}
	fmt.Println("配置文件 :", server.DefaultConfigPath)
	fmt.Println("监听     :", cfg.Listen)
	fmt.Println("管理员   :", cfg.AdminUsername)
	fmt.Println()

	// 1) 服务状态 + PID
	out, _ := exec.Command("systemctl", "is-active", "zk").Output()
	state := strings.TrimSpace(string(out))
	if state == "active" {
		pidOut, _ := exec.Command("systemctl", "show", "-p", "MainPID", "--value", "zk").Output()
		pid := strings.TrimSpace(string(pidOut))
		fmt.Printf("[✓] 服务运行中 (pid=%s)\n", pid)
	} else {
		fmt.Printf("[✗] 服务未运行 (状态=%s)\n", state)
	}

	// 2) 端口监听检查(尝试绑定,失败=已被占用=运行中)
	listenAddr := cfg.Listen
	if strings.HasPrefix(listenAddr, ":") {
		listenAddr = "127.0.0.1" + listenAddr
	}
	port := cfg.Listen
	if ln, err := net.Listen("tcp", listenAddr); err != nil {
		fmt.Printf("[✓] 端口 %s 监听中\n", port)
	} else {
		ln.Close()
		fmt.Printf("[✗] 端口 %s 未监听\n", port)
	}

	// 3) SQLite 文件检查
	dbPath := cfg.DBPath()
	if fi, err := os.Stat(dbPath); err == nil {
		sizeMB := float64(fi.Size()) / 1024 / 1024
		fmt.Printf("[✓] SQLite 可读 (%s, %.1f MB)\n", dbPath, sizeMB)
	} else {
		fmt.Printf("[✗] SQLite 文件不存在: %s\n", dbPath)
	}

	// 4) 磁盘剩余(使用 syscall.Statfs 仅在 Linux 可用,用 df 替代)
	dfOut, err := exec.Command("df", "-BG", "--output=avail", cfg.DataDir).Output()
	if err == nil {
		lines := strings.Split(strings.TrimSpace(string(dfOut)), "\n")
		if len(lines) >= 2 {
			avail := strings.TrimSpace(lines[1])
			fmt.Printf("[✓] 磁盘剩余 %s (%s)\n", avail, cfg.DataDir)
		}
	}

	// 5) 面板可访问性
	panelURL := "http://127.0.0.1" + cfg.Listen
	client := http.Client{Timeout: 2 * time.Second}
	req, _ := http.NewRequest(http.MethodGet, panelURL+"/api/config", nil)
	req.SetBasicAuth(cfg.AdminUsername, cfg.AdminPassword)
	resp, err := client.Do(req)
	if err == nil && resp.StatusCode/100 == 2 {
		resp.Body.Close()
		fmt.Printf("[✓] 主控可访问: %s\n", panelURL)
	} else {
		if resp != nil {
			resp.Body.Close()
		}
		fmt.Printf("[✗] 主控不可访问: %v\n", err)
	}
}

func cmdUninstall() {
	checkRoot()
	fmt.Println("⚠️  即将卸载主控,包括:")
	fmt.Println("  · 停止 zk 服务")
	fmt.Println("  · 删除 /etc/zk, /var/lib/zk")
	fmt.Println("  · 数据库与所有节点数据将被删除")
	fmt.Print("\n继续? [y/N]: ")
	r := bufio.NewReader(os.Stdin)
	line, _ := r.ReadString('\n')
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y") {
		fmt.Println("已取消")
		return
	}

	exec.Command("systemctl", "stop", "zk").Run()
	exec.Command("systemctl", "disable", "zk").Run()
	os.Remove("/etc/systemd/system/zk.service")
	exec.Command("systemctl", "daemon-reload").Run()
	os.RemoveAll("/etc/zk")
	os.RemoveAll("/var/lib/zk")
	os.RemoveAll("/var/log/zk")

	fmt.Println("✅ 已卸载")
	fmt.Println("如需删除二进制: sudo rm /usr/local/bin/zk")
}

func showMenu() {
	for {
		fmt.Printf(banner, shared.Version)
		cfg, err := server.LoadOrCreateConfig(server.DefaultConfigPath)
		if err == nil {
			out, _ := exec.Command("systemctl", "is-active", "zk").Output()
			state := strings.TrimSpace(string(out))
			fmt.Println("  状态  :", state)
			fmt.Println("  监听  :", cfg.Listen)
			fmt.Println("  访问  : http://<本机>:" + strings.TrimPrefix(cfg.Listen, ":"))
		} else {
			fmt.Println("  状态  : 未配置")
		}

		fmt.Println()
		fmt.Println("  [1] 查看状态")
		fmt.Println("  [2] 启动")
		fmt.Println("  [3] 停止")
		fmt.Println("  [4] 重启")
		fmt.Println("  [5] 更新到最新版本")
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
			cmdSystemctl("start", "zk")
		case "3":
			cmdSystemctl("stop", "zk")
		case "4":
			cmdSystemctl("restart", "zk")
		case "5":
			cmdUpdate()
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
