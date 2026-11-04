package wasm

import (
	"context"
	"encoding/json"
)

// HTTPForTest runs one http_request as the plugin would during a List call
// and returns the JSON response the plugin would receive. It lets tests
// see the response headers and validators, which the hostile plugin does
// not return.
func (s *Source) HTTPForTest(ctx context.Context, req []byte) []byte {
	ctx = context.WithValue(ctx, callKey{}, &callState{allowNet: true})
	out, err := json.Marshal(s.handleHTTP(ctx, req))
	if err != nil {
		panic(err)
	}
	return out
}
