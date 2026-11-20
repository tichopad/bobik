package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestMountExceptions(t *testing.T) {
	vault := t.TempDir()
	for _, dir := range []string{"team", "team/ready", "team/ready/private"} {
		if err := os.Mkdir(filepath.Join(vault, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("team", filepath.Join(vault, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vault, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, rules := range [][]Mount{
		{{"/team", Hide}}, {{"team/../team", Hide}}, {{"alias", Hide}}, {{"missing", Hide}},
		{{"team", Hide}, {"team/ready", Pin}}, {{"team", Hide}, {"team", ReadOnly}}, {{"file", Pin}},
	} {
		got, err := prepareVault(t.Context(), vault, rules)
		got.close()
		if err == nil {
			t.Fatalf("accepted invalid mounts: %+v", rules)
		}
	}
	got, err := prepareVault(t.Context(), vault, []Mount{{"team", ReadOnly}, {"team/ready/private", Hide}, {"team/ready", Pin}})
	if err != nil {
		t.Fatal(err)
	}
	defer got.close()
	if !got.wants["/notes/team"].readOnly || !got.wants["/notes/team/ready"].readOnly ||
		got.wants["/notes/team/ready/private"].mask == nil || got.wants["/notes"].readOnly {
		t.Fatalf("unexpected mount modes: %v", got.wants)
	}
}

func TestVaultLockHonorsCancellation(t *testing.T) {
	vault := t.TempDir()
	a, err := prepareVault(t.Context(), vault, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	b, err := prepareVault(ctx, vault, nil)
	b.close()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lock didn't wait: %v", err)
	}
}

func TestInspectVaultRejectsAliasesAndIPC(t *testing.T) {
	for _, tc := range []struct {
		name string
		make func(vault, other string) error
		want string
	}{
		{"hardlink to outside", func(v, o string) error { return os.Link(o, v) }, "multiply linked"},
		{"FIFO", func(v, _ string) error { return unix.Mkfifo(v, 0o600) }, "special file"},
		{"symlink", func(v, o string) error { return os.Symlink(o, v) }, "symlink"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			vault := filepath.Join(root, "vault")
			if err := os.Mkdir(vault, 0o700); err != nil {
				t.Fatal(err)
			}
			other := filepath.Join(root, "host")
			if err := os.WriteFile(other, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := tc.make(filepath.Join(vault, "entry"), other); err != nil {
				t.Fatal(err)
			}
			dir, err := os.Open(vault)
			if err != nil {
				t.Fatal(err)
			}
			defer dir.Close()
			err = inspectVault(t.Context(), dir, "/notes", nil)
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("inspection = %v, wanted %q", err, tc.want)
			}
		})
	}
}

func TestMountTableRejectsExtraAndPropagatedMounts(t *testing.T) {
	profile := runtimeProfile{binds: map[string]string{"/usr/bin": "/usr/bin"}}
	mask, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer mask.Close()
	notes := notesProfile{wants: map[string]mountWant{
		"/notes": {}, "/notes/private": {readOnly: true, mask: mask},
	}}
	good := []mountEntry{
		{point: "/", nodev: true, nosuid: true, readOnly: true, fsType: "tmpfs"},
		{point: "/dev/null", nosuid: true},
		{point: "/usr/bin", nodev: true, nosuid: true, readOnly: true, fsType: "ext4"},
		{point: "/notes", nodev: true, nosuid: true, fsType: "ext4"},
		{point: "/notes/private", nodev: true, nosuid: true, readOnly: true, fsType: "tmpfs"},
	}
	if err := verifyMountTable(good, profile, notes); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range [][]mountEntry{
		// Recursive binds include submounts, even those shadowed by an exception.
		append(append([]mountEntry{}, good...), mountEntry{point: "/notes/private/submount", nodev: true}),
		append(append([]mountEntry{}, good...), mountEntry{point: "/usr/bin/submount", readOnly: true}),
		append(append([]mountEntry{}, good...), good[len(good)-1]),
		good[:len(good)-1],
	} {
		if err := verifyMountTable(invalid, profile, notes); err == nil {
			t.Fatalf("accepted unsafe mount table: %v", invalid)
		}
	}
	good[3].nonPrivate = true
	if err := verifyMountTable(good, profile, notes); err == nil {
		t.Fatal("accepted propagated notes mount")
	}
	good[3].nonPrivate = false
	good[3].fsType = "proc"
	if err := verifyMountTable(good, profile, notes); err == nil {
		t.Fatal("accepted a host procfs as the vault mount")
	}
}

func TestBindAliasesUseFilesystemCoordinates(t *testing.T) {
	mounts := []mountEntry{
		{point: "/", device: "1:1", root: "/"},
		{point: "/example/deep", device: "1:1", root: "/real"},
		{point: "/runtime-alias", device: "1:1", root: "/usr/lib/test"},
		{point: "/example/deep/mounted", device: "2:2", root: "/"},
	}
	exposed := []string{"/example/deep", "/usr/lib"}
	for _, private := range []string{"/real/missing/config", "/runtime-alias/key"} {
		if err := checkPrivateCoordinates(mounts, exposed, private); err == nil {
			t.Fatalf("missed alias: %s", private)
		}
	}
	if err := checkPrivateCoordinates(mounts, exposed, "/real-other/config"); err != nil {
		t.Fatal(err)
	}
	if err := rejectSubmounts(mounts, "/example/deep"); err == nil {
		t.Fatal("missed host submount")
	}
}

func TestPrivatePathUsesAncestorIdentities(t *testing.T) {
	root := t.TempDir()
	vault := filepath.Join(root, "vault")
	if err := os.Mkdir(vault, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(vault, alias); err != nil {
		t.Fatal(err)
	}
	if err := checkPrivateIdentities([]string{vault}, filepath.Join(alias, "missing", "key")); err == nil {
		t.Fatal("missed ancestor alias for a private path that does not exist")
	}
	if err := checkPrivateIdentities([]string{vault}, filepath.Join(root, "missing", "key")); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkInspectVault(b *testing.B) {
	vault := b.TempDir()
	for d := range 100 {
		dir := filepath.Join(vault, fmt.Sprint(d))
		if err := os.Mkdir(dir, 0o700); err != nil {
			b.Fatal(err)
		}
		for n := range 100 {
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprint(n)), nil, 0o600); err != nil {
				b.Fatal(err)
			}
		}
	}
	b.ResetTimer()
	for b.Loop() {
		dir, err := os.Open(vault)
		if err != nil {
			b.Fatal(err)
		}
		if err := inspectVault(b.Context(), dir, "/notes", nil); err != nil {
			_ = dir.Close()
			b.Fatal(err)
		}
		_ = dir.Close()
	}
	b.ReportMetric(10_000, "entries/op")
}
