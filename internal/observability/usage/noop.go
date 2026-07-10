package usage

import "context"

type NoopObserver struct{}

func (NoopObserver) Begin(ctx context.Context, _ InteractionDimensions) (InteractionSpan, context.Context) {
	return NoopSpan{}, ctx
}

type NoopSpan struct{}

func (NoopSpan) SetExtension(any)             {}
func (NoopSpan) AddAnnotation(string, string) {}
func (NoopSpan) Finish(InteractionOutcome)    {}
