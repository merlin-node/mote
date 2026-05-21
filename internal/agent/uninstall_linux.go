//go:build linux

package agent

import "syscall"

func sysProcAttrDetach() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Setsid: true, // 新会话,脱离父进程的进程组
	}
}
