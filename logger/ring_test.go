package logger

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

func TestRingWrapsAndQueries(t *testing.T) {
	r := NewRing(3)
	var out bytes.Buffer
	log := slog.New(Tee(slog.NewTextHandler(&out, nil), r))
	for i := 1; i <= 5; i++ {
		log.Info(fmt.Sprintf("msg %d", i), "i", i)
	}
	entries, last := r.Query(0, slog.LevelDebug, "", 0)
	if last != 5 || len(entries) != 3 || entries[0].ID != 3 || entries[2].Message != "msg 5" {
		t.Fatalf("entries=%+v last=%d", entries, last)
	}
	if entries[1].Attrs["i"] != "4" {
		t.Fatalf("attrs: %v", entries[1].Attrs)
	}
	if got, _ := r.Query(4, slog.LevelDebug, "", 0); len(got) != 1 || got[0].ID != 5 {
		t.Fatalf("since: %+v", got)
	}
	if got, _ := r.Query(0, slog.LevelDebug, "MSG 4", 0); len(got) != 1 {
		t.Fatalf("q: %+v", got)
	}
	if got, _ := r.Query(0, slog.LevelWarn, "", 0); len(got) != 0 {
		t.Fatalf("level: %+v", got)
	}
	if got, _ := r.Query(0, slog.LevelDebug, "", 2); len(got) != 2 || got[1].ID != 5 {
		t.Fatalf("limit keeps newest: %+v", got)
	}
	// Terminal output is unchanged by the tee.
	if strings.Count(out.String(), "msg ") != 5 {
		t.Fatalf("wrapped handler output: %q", out.String())
	}
}

func TestTeeGroupsAndLevelGate(t *testing.T) {
	r := NewRing(10)
	log := slog.New(Tee(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelInfo}), r))
	log.Debug("hidden")
	log.WithGroup("req").With("id", 7).Info("x", "path", "/a\x1b[31m")
	entries, _ := r.Query(0, slog.LevelDebug, "", 0)
	if len(entries) != 1 {
		t.Fatalf("debug must be gated by the wrapped handler: %+v", entries)
	}
	a := entries[0].Attrs
	if a["req.id"] != "7" || a["req.path"] != `/a\x1b[31m` {
		t.Fatalf("attrs: %v", a)
	}
}

func TestRingConcurrent(t *testing.T) {
	r := NewRing(100)
	log := slog.New(Tee(slog.NewTextHandler(&bytes.Buffer{}, nil), r))
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				log.Info("m")
				r.Query(0, slog.LevelDebug, "m", 10)
			}
		}()
	}
	wg.Wait()
	if _, last := r.Query(0, slog.LevelDebug, "", 1); last != 1600 {
		t.Fatalf("last id = %d", last)
	}
}

func TestRingTruncatesMessageAndAttrs(t *testing.T) {
	r := NewRing(4)
	log := slog.New(Tee(slog.NewTextHandler(&bytes.Buffer{}, nil), r))
	huge := strings.Repeat("m", 1<<20)
	log.Info(huge, "v", huge)
	entries, _ := r.Query(0, slog.LevelDebug, "", 0)
	if len(entries) != 1 {
		t.Fatalf("entries: %d", len(entries))
	}
	e := entries[0]
	if want := strings.Repeat("m", maxValueLen) + "…"; e.Message != want {
		t.Fatalf("message not capped: %d bytes", len(e.Message))
	}
	if len(e.Attrs["v"]) != maxValueLen+len("…") {
		t.Fatalf("attr not capped: %d bytes", len(e.Attrs["v"]))
	}
	log.Info("short")
	if entries, _ := r.Query(1, slog.LevelDebug, "", 0); len(entries) != 1 || entries[0].Message != "short" {
		t.Fatalf("short message altered: %+v", entries)
	}
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{"": slog.LevelDebug, "info": slog.LevelInfo, "WARN": slog.LevelWarn, "error": slog.LevelError} {
		if got, ok := ParseLevel(in); !ok || got != want {
			t.Errorf("ParseLevel(%q) = %v %v", in, got, ok)
		}
	}
	if _, ok := ParseLevel("loud"); ok {
		t.Error("ParseLevel accepted junk")
	}
}
