package logsearch

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// cursorVersion prefixes the cursor's payload, so the format can change
// without a client mistaking an old cursor for a new one.
const cursorVersion = "v1:"

// errCursor is returned for a cursor this API did not issue.
var errCursor = errors.New("invalid cursor")

// encodeCursor returns the opaque cursor that resumes a backward search just
// before t: the time of the last entry of the page.
func encodeCursor(t time.Time) string {
	payload := cursorVersion + strconv.FormatInt(t.UnixNano(), 10)
	return base64.RawURLEncoding.EncodeToString([]byte(payload))
}

// decodeCursor returns the time a cursor resumes before.
func decodeCursor(c string) (time.Time, error) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %w", errCursor, err)
	}
	payload, ok := strings.CutPrefix(string(raw), cursorVersion)
	if !ok {
		return time.Time{}, errCursor
	}
	ns, err := strconv.ParseInt(payload, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %w", errCursor, err)
	}
	return time.Unix(0, ns).UTC(), nil
}
