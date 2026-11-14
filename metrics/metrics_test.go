package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMiddlewareCounts(t *testing.T) {
	reg := New()
	h := Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		switch r.URL.Path {
		case "/err":
			http.Error(w, "boom", http.StatusInternalServerError)
		case "/missing":
			w.WriteHeader(http.StatusNotFound)
		case "/flush":
			w.Write([]byte("ab"))
			w.(http.Flusher).Flush()
			if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
				t.Errorf("ResponseController through wrapper: %v", err)
			}
		default:
			w.Write([]byte("hello"))
		}
	}), reg)
	srv := httptest.NewServer(h)
	defer srv.Close()

	for _, p := range []string{"/", "/err", "/missing", "/flush"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/", strings.NewReader("12345678"))
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	req, _ = http.NewRequest("PROPFIND", srv.URL+"/", nil)
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()

	s := reg.Snapshot()
	if s.Requests != 6 || s.Err5xx != 1 || s.Err4xx != 1 || s.BytesIn != 8 {
		t.Fatalf("snapshot: %+v", s)
	}
	if s.BytesOut != int64(5+len("boom\n")+2+5+5) {
		t.Fatalf("bytes out: %d", s.BytesOut)
	}
	if s.ByMethod["GET"] != 4 || s.ByMethod["PUT"] != 1 || s.ByMethod["OTHER"] != 1 {
		t.Fatalf("by method: %v", s.ByMethod)
	}
	if len(s.Series) != SeriesLen || s.Series[SeriesLen-1].Requests != 6 || s.Series[SeriesLen-1].Errors != 2 {
		t.Fatalf("series tail: %+v", s.Series[SeriesLen-1])
	}
}

func TestSeriesRollsOver(t *testing.T) {
	reg := New()
	now := time.Unix(1_800_000_000, 0)
	reg.now = func() time.Time { return now }
	reg.Record("GET", 200, 1, 1)
	now = now.Add(2 * time.Minute)
	reg.Record("GET", 500, 0, 0)
	s := reg.Snapshot()
	last, prev, prev2 := s.Series[SeriesLen-1], s.Series[SeriesLen-2], s.Series[SeriesLen-3]
	if last.Requests != 1 || last.Errors != 1 || prev.Requests != 0 || prev2.Requests != 1 {
		t.Fatalf("series: %+v %+v %+v", prev2, prev, last)
	}
	// An hour later the old slots are stale and reported as zero.
	now = now.Add(time.Hour)
	reg.Record("GET", 200, 0, 0)
	s = reg.Snapshot()
	total := int64(0)
	for _, p := range s.Series {
		total += p.Requests
	}
	if total != 1 || s.Requests != 3 {
		t.Fatalf("after an hour: total=%d requests=%d", total, s.Requests)
	}
}
