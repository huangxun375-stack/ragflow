package archiveartifact

import (
	"context"
	"ragflow/internal/parser/codearchive"
)

type transformKey struct{}

// WithSourceTransform is the extension point for a future preprocessing
// requirement. Absence means identity; no redaction is enabled by this release.
func WithSourceTransform(ctx context.Context, transform codearchive.SourceTransform) context.Context {
	return context.WithValue(ctx, transformKey{}, transform)
}

func SourceTransformFrom(ctx context.Context) codearchive.SourceTransform {
	transform, _ := ctx.Value(transformKey{}).(codearchive.SourceTransform)
	return transform
}
