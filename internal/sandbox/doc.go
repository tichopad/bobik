// Package sandbox runs Bash searches in a Linux Bubblewrap sandbox. Source
// supplies the authorized view: no live vault path is ever mounted.
//
// Deployment requires a maintained, non-setuid Bubblewrap with --json-status-fd
// and --disable-userns, util-linux prlimit, and distro-packaged Bash, ripgrep,
// grep, coreutils and sed in /usr/bin (or /bin). The runtime currently supports
// linux/amd64 and linux/arm64 with standard distro library locations. Run
// Preflight as the deployment user and under the deployment's actual security
// policy. In particular, mounting and unprivileged namespaces must be allowed,
// including Bubblewrap's configuration of loopback in its new network namespace
// (RTM_NEWADDR and RTM_NEWLINK). There's no host-network or unsandboxed fallback.
//
// Put Bobik and its descendants in a deployment cgroup with memory.max and
// pids.max for hard aggregate limits. The sandbox's inherited rlimits are
// per-process, not job-wide. Set TMPDIR to a trusted disk-backed directory with
// sufficient space; the staging code uses os.TempDir and respects host mounts.
//
// Searches see independent per-call copies, not an atomic snapshot. Empty
// directories are omitted. Hidden files and ignore files affect search behavior
// only if the Source exposes them; for example, an excluded .gitignore can
// change ripgrep results. The view is read-only and there is no writable scratch
// or procfs. Pipelines and command substitutions work, but features requiring
// /dev/fd, such as Bash process substitution, are not supported.
package sandbox
