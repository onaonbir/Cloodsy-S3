package image

import "context"

// Limiter bounds the number of concurrent image decodes/encodes so a burst of
// transform requests cannot exhaust memory. A nil *Limiter never blocks.
type Limiter struct {
	sem chan struct{}
}

// NewLimiter creates a limiter allowing n concurrent operations (n <= 0 → 4).
func NewLimiter(n int) *Limiter {
	if n <= 0 {
		n = 4
	}
	return &Limiter{sem: make(chan struct{}, n)}
}

// Acquire blocks until a slot is free or ctx is done. It returns false when
// the context expired first.
func (l *Limiter) Acquire(ctx context.Context) bool {
	if l == nil {
		return true
	}
	select {
	case l.sem <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

// TryAcquire takes a slot without blocking.
func (l *Limiter) TryAcquire() bool {
	if l == nil {
		return true
	}
	select {
	case l.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

// Release frees a slot taken by Acquire/TryAcquire.
func (l *Limiter) Release() {
	if l == nil {
		return
	}
	<-l.sem
}

// Quantize rounds the requested width/height up to a multiple of step so the
// number of distinct cached variants per object stays bounded. step <= 1 keeps
// exact dimensions. Quality is rounded to a multiple of 5.
func (p Params) Quantize(step int) Params {
	if step > 1 {
		if p.Width > 0 {
			p.Width = roundUp(p.Width, step)
			if p.Width > MaxDimension {
				p.Width = MaxDimension
			}
		}
		if p.Height > 0 {
			p.Height = roundUp(p.Height, step)
			if p.Height > MaxDimension {
				p.Height = MaxDimension
			}
		}
	}
	if p.Quality > 0 {
		p.Quality = roundUp(p.Quality, 5)
		if p.Quality > 100 {
			p.Quality = 100
		}
	}
	return p
}

func roundUp(v, step int) int {
	if step <= 1 || v%step == 0 {
		return v
	}
	return v + (step - v%step)
}
