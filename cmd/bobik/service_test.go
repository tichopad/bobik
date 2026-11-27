package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestInstallServiceReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "user", serviceName)
	var fragment, state string
	var actions []string
	var failEnable bool
	run := func(name string, args ...string) (string, error) {
		if name != "systemctl" {
			t.Fatalf("unexpected command: %s %v", name, args)
		}
		action := args[1]
		switch action {
		case "show":
			return fmt.Sprintf("FragmentPath=%s\nNeedDaemonReload=no\nUnitFileState=%s", fragment, state), nil
		case "daemon-reload":
			if _, err := os.Stat(path); err != nil {
				return "", err
			}
			fragment = path
		case "enable":
			if failEnable {
				return "", errors.New("enable failed")
			}
			state = "enabled"
		}
		actions = append(actions, action)
		return "", nil
	}
	original := []byte("original settings")
	updated := []byte("updated settings")

	failEnable = true
	if err := installService(path, original, false, false, run); err == nil {
		t.Fatal("install reported success after enable failed")
	}
	failEnable = false
	actions = nil
	if err := installService(path, original, false, false, run); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actions, []string{"enable"}) {
		t.Fatalf("repair actions: %v", actions)
	}
	actions = nil
	if err := installService(path, original, false, false, run); err != nil || len(actions) != 0 {
		t.Fatalf("identical install: err=%v actions=%v", err, actions)
	}
	if err := installService(path, updated, false, true, run); err == nil || len(actions) != 0 {
		t.Fatalf("unapproved replacement: err=%v actions=%v", err, actions)
	}
	if content, err := os.ReadFile(path); err != nil || !bytes.Equal(content, original) {
		t.Fatalf("existing unit: %q, %v", content, err)
	}
	if err := installService(path, updated, true, true, run); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actions, []string{"daemon-reload", "restart"}) {
		t.Fatalf("replacement actions: %v", actions)
	}
	if content, err := os.ReadFile(path); err != nil || !bytes.Equal(content, updated) {
		t.Fatalf("replacement unit: %q, %v", content, err)
	}
}

func TestServiceRejectsLingering(t *testing.T) {
	run := func(name string, args ...string) (string, error) {
		if name != "loginctl" {
			t.Fatalf("unexpected command: %s %v", name, args)
		}
		return "yes", nil
	}
	err := requireNonLingering(run)
	if err == nil || !strings.Contains(err.Error(), "lingering is enabled") {
		t.Fatalf("install error: %v", err)
	}
}

func TestJournalEntryEncodesMultilinePaths(t *testing.T) {
	path := "/vault\nBOBIK_OUTCOME=success"
	entry := journalEntry("run_finished", "Run failed", map[string]string{
		"OUTCOME": "failed", "AUDIT_PATH": path,
	})
	want := "PRIORITY=4\nSYSLOG_IDENTIFIER=bobik\nMESSAGE=Run failed\nBOBIK_EVENT=run_finished\n" +
		"BOBIK_AUDIT_PATH\n" + string([]byte{byte(len(path)), 0, 0, 0, 0, 0, 0, 0}) + path +
		"\nBOBIK_OUTCOME=failed\n"
	if string(entry) != want {
		t.Fatalf("journal entry = %q, want %q", entry, want)
	}
}
