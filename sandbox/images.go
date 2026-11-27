package sandbox

import (
	"context"

	m "github.com/superradcompany/microsandbox/sdk/go"
)

// RemoveMsbImage removes ref from microsandbox's image cache, unless a
// sandbox still uses it (inUse, and it stays). A missing image is no
// error.
func RemoveMsbImage(ctx context.Context, ref string) (inUse bool, err error) {
	err = m.Image.Remove(ctx, ref, false)
	switch {
	case err == nil, m.IsKind(err, m.ErrImageNotFound):
		return false, nil
	case m.IsKind(err, m.ErrImageInUse):
		return true, nil
	}
	return false, err
}

// MsbImageBytes is the size of every image in microsandbox's cache, as
// it counts them.
func MsbImageBytes(ctx context.Context) (int64, error) {
	l, err := m.Image.List(ctx)
	if err != nil {
		return 0, err
	}
	var n int64
	for _, h := range l {
		if s := h.SizeBytes(); s != nil {
			n += *s
		}
	}
	return n, nil
}
