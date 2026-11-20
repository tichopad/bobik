package sandbox

import (
	"encoding/binary"
	"fmt"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// Deny sockets (including VSOCK), filesystem aliases and IPC nodes. Block
// fallocate and io_uring so they cannot sidestep file limits and filtering.
func syscallFilter() (*os.File, error) {
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
	}
	denied := []uint32{
		unix.SYS_SOCKET, unix.SYS_SOCKETPAIR, unix.SYS_IO_URING_SETUP,
		unix.SYS_LINKAT, unix.SYS_SYMLINKAT, unix.SYS_MKNODAT,
		unix.SYS_FALLOCATE,
	}
	if runtime.GOARCH == "amd64" {
		// Legacy x86-64 syscalls are not present in the arm64 syscall table.
		denied = append(denied, 86 /* link */, 88 /* symlink */, 133 /* mknod */)
	}
	for _, number := range denied {
		filter = append(filter, unix.SockFilter{Code: eq, Jf: 1, K: number},
			unix.SockFilter{Code: ret, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)})
	}
	filter = append(filter, unix.SockFilter{Code: ret, K: unix.SECCOMP_RET_ALLOW})
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
