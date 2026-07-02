package socialkit

import (
	"context"
	"regexp"
	"strings"
)

// noopEnricher returns no display data; handlers fall back to bare ids.
type noopEnricher struct{}

func (noopEnricher) UsersByIDs(context.Context, []string) (map[string]PublicUser, error) {
	return map[string]PublicUser{}, nil
}

// noopRecorder drops engagement signals (no discovery system wired).
type noopRecorder struct{}

func (noopRecorder) Reaction(context.Context, ReactionSignal) {}
func (noopRecorder) Post(context.Context, PostSignal)         {}

// unsupportedMediaStore rejects uploads (no MediaStore wired).
type unsupportedMediaStore struct{}

func (unsupportedMediaStore) Put(context.Context, string, []byte, string) (string, error) {
	return "", errUnsupportedMedia
}

// stripProcessor is the default ContentProcessor: strip HTML tags to a safe
// plain-text subset. Hosts plug in their own rich-text sanitizer.
type stripProcessor struct{}

var tagRe = regexp.MustCompile(`<[^>]*>`)

func (stripProcessor) Sanitize(_ context.Context, raw string) (string, error) {
	return strings.TrimSpace(tagRe.ReplaceAllString(raw, "")), nil
}
