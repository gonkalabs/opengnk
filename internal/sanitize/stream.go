package sanitize

import (
	"io"
	"strings"
)

// tokenPrefix and tokenSuffix are the delimiters used for placeholder tokens.
// The restoring reader must handle the case where a token is split across
// multiple SSE chunks.
const tokenPrefix = "«TOKEN_"
const tokenSuffix = "»"

// RestoringReader wraps an upstream SSE response body and replaces any
// placeholder tokens with their original values before the bytes reach the
// client. It handles tokens that are split across chunk boundaries by
// maintaining a small look-ahead buffer.
type RestoringReader struct {
	src    io.Reader
	tm     *TokenMap
	buf    []byte // buffered bytes not yet written to consumer
	srcEOF bool
}

// NewRestoringReader wraps src so that all «TOKEN_XXXXXX» markers are replaced
// with their originals from tm before being returned to the caller.
// If tm is nil or empty the original reader is returned unchanged.
func NewRestoringReader(src io.Reader, tm *TokenMap) io.Reader {
	if tm == nil || tm.IsEmpty() {
		return src
	}
	return &RestoringReader{src: src, tm: tm}
}

// Read implements io.Reader. It reads from the upstream, appends to the
// internal buffer, restores tokens in the safe portion of the buffer
// (everything except the last len(tokenPrefix)-1 bytes that might be the
// start of a split token), and copies the result into p.
func (r *RestoringReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	// If we have buffered output ready, drain it first.
	if len(r.buf) > 0 {
		n := copy(p, r.buf)
		r.buf = r.buf[n:]
		return n, nil
	}

	if r.srcEOF {
		return 0, io.EOF
	}

	// Read a chunk from upstream.
	tmp := make([]byte, len(p)*2)
	n, err := r.src.Read(tmp)
	if err == io.EOF {
		r.srcEOF = true
	} else if err != nil {
		return 0, err
	}

	chunk := tmp[:n]

	// Restore tokens in the chunk. When we are at EOF we can restore
	// everything; otherwise we hold back a tail that might be a partial token.
	var safe []byte
	if r.srcEOF {
		safe = chunk
	} else {
		// Hold back enough bytes to cover a partial token marker.
		// Worst case: "«TOKEN_000001" without the closing "»" is about 14 bytes.
		// Hold back 20 to be safe.
		const holdBack = 20
		if len(chunk) <= holdBack {
			// Too short to split safely; buffer everything and wait for more.
			r.buf = append(r.buf, chunk...)
			return r.Read(p)
		}
		safe = chunk[:len(chunk)-holdBack]
		r.buf = append(r.buf, chunk[len(chunk)-holdBack:]...)
	}

	restored := restoreBytes(safe, r.tm)

	// If we are at EOF also restore the held-back buffer.
	if r.srcEOF && len(r.buf) > 0 {
		tail := restoreBytes(r.buf, r.tm)
		r.buf = []byte(tail)
	}

	copied := copy(p, restored)
	if copied < len(restored) {
		// p was too small; prepend the overflow back to the buffer.
		remainder := []byte(string(restored[copied:]))
		r.buf = append(remainder, r.buf...)
	}
	return copied, nil
}

// restoreBytes applies token restoration to a byte slice.
func restoreBytes(b []byte, tm *TokenMap) []byte {
	s := string(b)
	for tok, orig := range tm.fromToken {
		s = strings.ReplaceAll(s, tok, orig)
	}
	return []byte(s)
}
