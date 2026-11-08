package inbox

import (
	"context"
	"errors"
	"log/slog"
)

// ErrFallback is returned by an executor whose failure should make the digest run on
// the fallback executor (DESIGN §23.5): no jobs worker online, the start not
// acknowledged, the claude run ending without a result (the subscription's usage
// limit exits 2 without one) or with is_error. Note says what happened; it is kept
// in the digest's error column.
type ErrFallback struct{ Note string }

func (e ErrFallback) Error() string { return e.Note }

// FallbackExecutor runs Primary and, when it fails with ErrFallback while the run's
// context is still alive, runs the same input on Secondary. The result then names
// Secondary as the executor and carries Primary's note.
type FallbackExecutor struct {
	Primary, Secondary Executor
	Log                *slog.Logger
}

// Name is the configured executor (the digest row says which one actually ran).
func (f FallbackExecutor) Name() string { return f.Primary.Name() }

func (f FallbackExecutor) Run(ctx context.Context, in DigestInput) (DigestResult, error) {
	res, err := f.Primary.Run(ctx, in)
	if res.Executor == "" {
		res.Executor = f.Primary.Name()
	}
	var fb ErrFallback
	if err == nil || !errors.As(err, &fb) || ctx.Err() != nil || f.Secondary == nil {
		return res, err
	}
	log := f.Log
	if log == nil {
		log = slog.Default()
	}
	log.Warn("inbox: digest falls back", "digest", in.DigestID, "from", f.Primary.Name(), "to", f.Secondary.Name(), "reason", fb.Note)
	res2, err2 := f.Secondary.Run(ctx, in)
	res2.Executor = f.Secondary.Name()
	note := f.Primary.Name() + " executor: " + fb.Note
	if res2.Note != "" {
		note += "; " + res2.Note
	}
	res2.Note = note
	return res2, err2
}
