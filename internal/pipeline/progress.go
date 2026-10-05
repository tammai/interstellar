package pipeline

import "context"

// Progress is how far the current stage has got, for the Schedule screen's live
// view. Unit names what is being counted -- sources, batches, topics -- because a
// bar reading 40 / 385 means nothing without it.
type Progress struct {
	Done  int    `json:"done"`
	Total int    `json:"total"`
	Unit  string `json:"unit"`
}

type progressKey struct{}

// withProgress attaches a progress sink to a run's context. It travels on the
// context rather than as a parameter because it is optional and observational: a
// stage reports into it if one is there and does nothing otherwise, which keeps
// every stage callable from a test without one.
func withProgress(ctx context.Context, fn func(Progress)) context.Context {
	return context.WithValue(ctx, progressKey{}, fn)
}

func reportProgress(ctx context.Context, done, total int, unit string) {
	if fn, ok := ctx.Value(progressKey{}).(func(Progress)); ok {
		fn(Progress{Done: done, Total: total, Unit: unit})
	}
}
