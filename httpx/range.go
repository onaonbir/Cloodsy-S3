package httpx

import (
	"strconv"
	"strings"
)

// ParseRange parses a single "bytes=start-end" range against size. Multi-range
// requests and unparsable ranges return ok=false (S3 then serves the whole
// object). An unsatisfiable range returns ok=false with start=-1.
func ParseRange(rangeHeader string, size int64) (start, end int64, ok bool) {
	spec, found := strings.CutPrefix(strings.TrimSpace(rangeHeader), "bytes=")
	if !found || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	first, last, found := strings.Cut(spec, "-")
	if !found {
		return 0, 0, false
	}
	first, last = strings.TrimSpace(first), strings.TrimSpace(last)
	if first == "" {
		// suffix range: -N means last N bytes
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil || n <= 0 {
			return -1, 0, false
		}
		if size == 0 {
			return -1, 0, false
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true
	}
	s, err := strconv.ParseInt(first, 10, 64)
	if err != nil || s < 0 {
		return 0, 0, false
	}
	e := size - 1
	if last != "" {
		e, err = strconv.ParseInt(last, 10, 64)
		if err != nil || e < s {
			return 0, 0, false
		}
		if e >= size {
			e = size - 1
		}
	}
	if s >= size {
		return -1, 0, false
	}
	return s, e, true
}
