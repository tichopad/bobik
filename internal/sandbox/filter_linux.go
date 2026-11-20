package sandbox

import (
	"encoding/binary"
	"fmt"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// In addition to the private network namespace, deny socket creation outright
// (some address families, such as VSOCK, aren't isolated by network namespaces).
// Deny io_uring setup so operations cannot bypass these syscall restrictions.
// No socket descriptors are inherited.
func socketFilter() (*os.File, error) {
	var arch uint32
	switch runtime.GOARCH {
	case "amd64":
		arch = unix.AUDIT_ARCH_X86_64
	case "arm64":
		arch = unix.AUDIT_ARCH_AARCH64
	default:
		return nil, fmt.Errorf("unsupported seccomp architecture: %s", runtime.GOARCH)
	}
	const (
		load = unix.BPF_LD | unix.BPF_W | unix.BPF_ABS
		eq   = unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K
		ge   = unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K
		ret  = unix.BPF_RET | unix.BPF_K
		kill = unix.SECCOMP_RET_KILL_PROCESS
	)
	filter := []unix.SockFilter{
		{Code: load, K: 4}, // seccomp_data.arch
		{Code: eq, Jt: 1, K: arch},
		{Code: ret, K: kill},
		{Code: load, K: 0},               // seccomp_data.nr
		{Code: ge, Jf: 1, K: 0x40000000}, // reject x32 and invalid negative syscall numbers
		{Code: ret, K: kill},
		{Code: eq, Jt: 3, K: unix.SYS_SOCKET},
		{Code: eq, Jt: 2, K: unix.SYS_SOCKETPAIR},
		{Code: eq, Jt: 1, K: unix.SYS_IO_URING_SETUP},
		{Code: ret, K: unix.SECCOMP_RET_ALLOW},
		{Code: ret, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	writeErr := binary.Write(writer, binary.NativeEndian, filter)
	closeErr := writer.Close()
	if writeErr != nil || closeErr != nil {
		_ = reader.Close()
		if writeErr != nil {
			return nil, writeErr
		}
		return nil, closeErr
	}
	return reader, nil
}
