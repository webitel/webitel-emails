// Package emailhtml makes the HTML body of an email safe to show and links its
// inline images. It is the only place where a stored HTML body becomes output.
package emailhtml

import (
	"net/url"
	"strings"
)

// CIDReference returns the Content-ID of a "cid:" URL: the scheme ignores case,
// the value is kept as written (RFC 2392).
func CIDReference(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if len(value) < len("cid:") || !strings.EqualFold(value[:len("cid:")], "cid:") {
		return "", false
	}

	return value[len("cid:"):], true
}

// MatchesContentID reports whether a reference, as is or URL-decoded, names a part's
// Content-ID. Intake and the read API share it.
func MatchesContentID(reference, contentID string) bool {
	if reference == "" || contentID == "" {
		return false
	}
	if reference == contentID {
		return true
	}
	if decoded, err := url.PathUnescape(reference); err == nil && decoded == contentID {
		return true
	}
	if decoded, err := url.QueryUnescape(reference); err == nil && decoded == contentID {
		return true
	}

	return false
}
