package upstream

import (
	"context"
	"encoding/json"
	"net/http"
)

// Upstream is the contract the API handler depends on. It abstracts away the
// transport (ECDSA-signed node requests vs. Bearer-token devshard gateway) so
// the handler never needs to know how requests are authenticated or routed.
type Upstream interface {
	FetchModels(ctx context.Context) ([]json.RawMessage, error)
	Do(ctx context.Context, method, path string, payload []byte) ([]byte, int, error)
	DoStream(ctx context.Context, method, path string, payload []byte) (*http.Response, error)
}
