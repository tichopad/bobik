package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"maps"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/tichopad/bobik/internal/watch"
)

func journalLogger(fallback io.Writer) watch.ServiceLog {
	return func(event, message string, fields map[string]string) {
		if err := sendJournal("/run/systemd/journal/socket", journalEntry(event, message, fields)); err != nil {
			fmt.Fprintf(fallback, "bobik: %s: %s (journal fields unavailable: %v)\n", event, message, err)
		}
	}
}

func journalEntry(event, message string, fields map[string]string) []byte {
	var entry bytes.Buffer
	priority := "6"
	if fields["OUTCOME"] == "failed" || event == "watcher_error" || event == "retry_exhausted" {
		priority = "4"
	}
	appendJournalField(&entry, "PRIORITY", priority)
	appendJournalField(&entry, "SYSLOG_IDENTIFIER", "bobik")
	appendJournalField(&entry, "MESSAGE", message)
	appendJournalField(&entry, "BOBIK_EVENT", event)
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		appendJournalField(&entry, "BOBIK_"+name, fields[name])
	}
	return entry.Bytes()
}

func appendJournalField(entry *bytes.Buffer, name, value string) {
	entry.WriteString(name)
	if strings.ContainsRune(value, '\n') {
		entry.WriteByte('\n')
		entry.Write(binary.LittleEndian.AppendUint64(nil, uint64(len(value))))
	} else {
		entry.WriteByte('=')
	}
	entry.WriteString(value)
	entry.WriteByte('\n')
}

func sendJournal(socket string, entry []byte) error {
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		return err
	}
	_, err = conn.Write(entry)
	return err
}
