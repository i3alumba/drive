// Package mediaadmission bounds local decoders shared by preview and conversion.
package mediaadmission

import "context"

// Admission is an instance-scoped single-decoder token. Construct once in main.
// Release must follow process and source-I/O joins, not cancellation requests.
type Admission struct{ token chan struct{} }

func New() *Admission { return &Admission{token: make(chan struct{}, 1)} }

func (a *Admission) TryAcquire() bool {
	select {
	case a.token <- struct{}{}:
		return true
	default:
		return false
	}
}

func (a *Admission) Acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case a.token <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *Admission) Release() { <-a.token }
