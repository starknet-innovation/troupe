package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// avatarLimit caps a profile picture. Real ones are well under it, and the agent's model reads
// it: Venice accepted 8 MB images when measured (2026-10-14).
const avatarLimit = 5 << 20

// avatarClient fetches a profile picture; tests replace it to trust their own server.
var avatarClient = &http.Client{
	Timeout: 30 * time.Second,
	// A redirect stays on https, like the address given.
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if req.URL.Scheme != "https" {
			return errors.New("redirected away from https")
		}
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return nil
	},
}

// readAvatar reads a profile picture from a file or an https address: at most avatarLimit
// bytes, and a PNG or JPEG by its first bytes. It is never decoded here; the agent's own model
// looks at it. It returns the image and its extension (png or jpg).
func readAvatar(ctx context.Context, from string) ([]byte, string, error) {
	var source io.Reader
	if strings.Contains(from, "://") {
		u, err := url.Parse(from)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return nil, "", errors.New("--avatar: an address must start with https://")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, "", err
		}
		resp, err := avatarClient.Do(req)
		if err != nil {
			return nil, "", fmt.Errorf("--avatar: could not fetch it: %v", plainError(err))
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, "", fmt.Errorf("--avatar: the address answered %d", resp.StatusCode)
		}
		source = resp.Body
	} else {
		file, err := os.Open(from)
		if err != nil {
			return nil, "", fmt.Errorf("--avatar: %w", err)
		}
		defer file.Close()
		source = file
	}
	data, err := io.ReadAll(io.LimitReader(source, avatarLimit+1))
	if err != nil {
		return nil, "", fmt.Errorf("--avatar: %v", plainError(err))
	}
	switch {
	case len(data) > avatarLimit:
		return nil, "", fmt.Errorf("--avatar: the picture is larger than %d MiB", avatarLimit>>20)
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return data, "png", nil
	case bytes.HasPrefix(data, []byte{0xff, 0xd8, 0xff}):
		return data, "jpg", nil
	}
	return nil, "", errors.New("--avatar: the picture must be a PNG or a JPEG")
}

// plainError is an error's message without the address that may carry a token or a password.
func plainError(err error) string {
	var u *url.Error
	if errors.As(err, &u) {
		return u.Err.Error()
	}
	return err.Error()
}
