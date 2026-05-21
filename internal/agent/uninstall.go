package agent

import (
	"log"
	"os"
	"os/exec"
)

// UninstallScript fork 出一个 shell 脚本完成自毁。
// sleep 5 是为了让 agent 进程有时间退出,以及 systemctl stop 完成。
const UninstallScript = `#!/bin/bash
set +e
sleep 5
/bin/systemctl stop bk 2>/dev/null
/bin/systemctl disable bk 2>/dev/null
rm -f /etc/systemd/system/bk.service
/bin/systemctl daemon-reload 2>/dev/null
rm -rf /etc/bk /var/log/bk /opt/bk
rm -f /usr/local/bin/bk
rm -- "$0"
`

// triggerUninstall 在收到主控的卸载指令后,把脚本写到独占临时文件并 fork 执行。
// 注意:调用方必须保证 WS 连接已关闭、清理工作已完成,这里直接 Exit(0)。
func triggerUninstall() {
	// 用 os.CreateTemp 生成不可预测的文件名,避免符号链接/抢占竞态
	f, err := os.CreateTemp("", "bk-uninstall-*.sh")
	if err != nil {
		log.Printf("create uninstall tempfile failed: %v", err)
		return
	}
	scriptPath := f.Name()
	// 收紧权限
	if err := os.Chmod(scriptPath, 0700); err != nil {
		log.Printf("chmod uninstall script failed: %v", err)
		f.Close()
		os.Remove(scriptPath)
		return
	}
	if _, err := f.WriteString(UninstallScript); err != nil {
		log.Printf("write uninstall script failed: %v", err)
		f.Close()
		os.Remove(scriptPath)
		return
	}
	if err := f.Close(); err != nil {
		log.Printf("close uninstall script failed: %v", err)
		os.Remove(scriptPath)
		return
	}

	cmd := exec.Command("/bin/bash", scriptPath)
	// 与父进程脱钩,避免 systemd 把 bash 也收走
	cmd.SysProcAttr = sysProcAttrDetach()
	if err := cmd.Start(); err != nil {
		log.Printf("start uninstall script failed: %v", err)
		os.Remove(scriptPath)
		return
	}
	// 释放 PID,bash 由 init 接管
	_ = cmd.Process.Release()
	log.Println("uninstall script forked, exiting agent")
	os.Exit(0)
}

// DoLocalUninstall 是 `bk uninstall` CLI 命令的实现:同步执行卸载
func DoLocalUninstall() error {
	// 先停服务(忽略错误,可能没用 systemd)
	exec.Command("systemctl", "stop", "bk").Run()
	exec.Command("systemctl", "disable", "bk").Run()

	// 删 unit 文件
	os.Remove("/etc/systemd/system/bk.service")
	exec.Command("systemctl", "daemon-reload").Run()

	// 删数据与配置
	os.RemoveAll("/etc/bk")
	os.RemoveAll("/var/log/bk")
	os.RemoveAll("/opt/bk")

	return nil
}
