//go:build !linux

package agent

import "syscall"

func sysProcAttrDetach() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}
