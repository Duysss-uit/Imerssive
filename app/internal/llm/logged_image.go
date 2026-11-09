package llm

import (
	"context"
	"time"
)

// ImageCall is one finished drawing, as seen by a logging wrapper.
type ImageCall struct {
	Provider string // display name of the provider
	Model    string
	API      string // chat | images | sdwebui | comfyui
	Request  ImageRequest
	Image    Image // what came back (empty on error)
	Err      error // the server's error, refusal or explanation, in full
	Started  time.Time
	Duration time.Duration
}

// LoggedImager wraps im so every drawing is reported to record once it ends.
func LoggedImager(im Imager, provider, model, api string, record func(ImageCall)) Imager {
	return &loggedImager{im: im, provider: provider, model: model, api: api, record: record}
}

type loggedImager struct {
	im                   Imager
	provider, model, api string
	record               func(ImageCall)
}

func (l *loggedImager) Draw(ctx context.Context, r ImageRequest) (Image, error) {
	start := time.Now()
	img, err := l.im.Draw(ctx, r)
	l.record(ImageCall{Provider: l.provider, Model: l.model, API: l.api, Request: r, Image: img, Err: err,
		Started: start, Duration: time.Since(start)})
	return img, err
}
