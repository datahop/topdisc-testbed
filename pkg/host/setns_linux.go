//go:build linux && !amd64 && !386

package host

import "syscall"

const sysSetns = syscall.SYS_SETNS
