package credential

import "context"

type skipPersistKey struct{}

func WithSkipPersist(ctx context.Context) context.Context {
	return context.WithValue(ctx, skipPersistKey{}, true)
}

func shouldSkipPersist(ctx context.Context) bool {
	v, _ := ctx.Value(skipPersistKey{}).(bool)
	return v
}
