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

// ValidID reports whether s has the shape NewID produces.
//
// It is a shape check and not an existence check. The caller that needs it is
// the Telegram callback decoder, which has to tell a payload it cannot parse
// from one naming a row that is simply gone: the first is answered with a toast
// and no database work at all, and the second is store.ErrNotFound. The store
// lookup remains the real backstop — this only keeps obvious garbage from
// reaching a query.
func ValidID(s string) bool {
	_, err := ulid.ParseStrict(s)
	return err == nil
}
