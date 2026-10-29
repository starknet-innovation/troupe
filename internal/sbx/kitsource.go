package sbx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// AllowKitSource makes sbx accept kits from registry (troupe's registry setting, without a type).
// A fresh sbx allows kits only from docker.io/ (sbx 0.50.1), so troupe adds its registry to
// kit.allowedSources, as a path prefix that admits no one else's kits on the same host, and keeps
// every entry already there. It reports whether it added it. A setting that is not a list of
// entries (null or empty included, whose meaning sbx does not state) is never rewritten.
func (r Runner) AllowKitSource(ctx context.Context, registry string) (bool, error) {
	out, err := r.Output(ctx, Call{Args: []string{"settings", "get", "kit.allowedSources"}, Timeout: time.Minute})
	if err != nil {
		return false, err
	}
	var sources []string
	if err := json.Unmarshal(bytes.TrimSpace(out), &sources); err != nil || len(sources) == 0 {
		return false, errors.New("unreadable sbx setting kit.allowedSources")
	}
	prefix := strings.TrimSuffix(registry, "/") + "/"
	for _, s := range sources {
		if strings.HasPrefix(prefix, s) {
			return false, nil
		}
	}
	value, err := json.Marshal(append(sources, prefix))
	if err != nil {
		return false, err
	}
	if _, err := r.Output(ctx, Call{Args: []string{"settings", "set", "kit.allowedSources", string(value)}, Timeout: time.Minute}); err != nil {
		return false, err
	}
	return true, nil
}
