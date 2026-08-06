package domain

import "github.com/oklog/ulid/v2"

// NewID returns a fresh identifier. ULIDs sort chronologically as text, which
// means an ORDER BY id is an ORDER BY creation time and a primary-key index is
// already the index for "most recent first". They are also safe in a URL, so a
// row identifier can appear in a path without encoding.
//
// ulid.Make draws from a locked monotonic entropy source, so two calls in the
// same millisecond still order correctly and it is safe to call from any
// goroutine.
func NewID() string {
	return ulid.Make().String()
}
