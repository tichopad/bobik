package watch

import (
	"crypto/sha256"
	"reflect"
	"testing"
	"time"
)

var epoch = time.Unix(100, 0)

func digest(content string) [sha256.Size]byte { return sha256.Sum256([]byte(content)) }
func stillReady(string) bool                  { return true }

func TestScheduleQuietDeadlineAndDeletionContext(t *testing.T) {
	s := newSchedule(60*time.Second, time.Minute)
	if got := s.take(epoch, stillReady); !got.initial {
		t.Fatal("startup was not immediate")
	}
	s.remove("/vault/gone.md")
	if at := s.next(); !at.IsZero() {
		t.Fatalf("deletion scheduled a run for %v", at)
	}
	s.ready("/vault/z.md", digest("z"), epoch)
	if s.ready("/vault/z.md", digest("z"), epoch.Add(time.Second)) {
		t.Fatal("same bytes triggered a run")
	}
	s.ready("/vault/a.md", digest("a"), epoch.Add(10*time.Second))
	at := epoch.Add(70 * time.Second)
	if s.next() != at {
		t.Fatalf("new relevant edit should advance deadline to %v, got %v", at, s.next())
	}
	got := s.take(at, stillReady).changes("/vault")
	want := []ChangedFile{{Path: "a.md"}, {Path: "z.md"}, {Path: "gone.md", Deleted: true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changes = %#v, want %#v", got, want)
	}
}

func TestScheduleRevalidationAndRetries(t *testing.T) {
	s := newSchedule(0, time.Minute)
	s.take(epoch, stillReady)
	const note = "/vault/note.md"
	hash := digest("request")
	s.ready(note, hash, epoch)
	if work := s.take(epoch, func(string) bool { return false }); len(work.paths) != 0 {
		t.Fatal("unready request dispatched")
	}
	if !s.ready(note, hash, epoch) {
		t.Fatal("resubmitting bytes after a withdrawn request should dispatch")
	}
	first := s.take(epoch, stillReady)
	s.ignoreWrite(note, first)
	if gaveUp, retry := s.failed(first, epoch); !retry || len(gaveUp) != 0 {
		t.Fatal("first failure should schedule one retry, even if it wrote the note")
	}
	at := epoch.Add(time.Minute)
	if s.next() != at || len(s.take(at.Add(-time.Second), stillReady).paths) != 0 {
		t.Fatal("retry dispatched before its scheduled time")
	}
	second := s.take(at, stillReady)
	if gaveUp, retry := s.failed(second, at); retry || !reflect.DeepEqual(gaveUp, []string{note}) {
		t.Fatalf("second failure gave up on %v, retry=%t", gaveUp, retry)
	}
	if s.ready(note, hash, at) || !s.ready(note, digest("edited request"), at) {
		t.Fatal("only changed content may re-trigger after give-up")
	}
	if _, retry := s.failed(s.take(at, stillReady), at); !retry {
		t.Fatal("the newly edited request should get its own retry")
	}
	s.unready(note)
	if !s.next().IsZero() {
		t.Fatal("withdrawing the request should cancel its retry")
	}
}

func TestScheduleRetryCoalescesWithNewWork(t *testing.T) {
	s := newSchedule(time.Second, time.Minute)
	work := s.take(epoch, stillReady)
	if _, retry := s.failed(work, epoch); !retry {
		t.Fatal("startup should also retry")
	}
	s.ready("/vault/new.md", digest("new"), epoch)
	got := s.take(epoch.Add(time.Second), stillReady)
	if !got.initial || !got.initialRetried || len(got.paths) != 1 {
		t.Fatalf("startup retry did not coalesce with fresh work: %#v", got)
	}
}
