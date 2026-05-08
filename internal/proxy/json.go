// Package proxy — json.go contains a single tiny helper. It exists in
// its own file so the proxy doesn't pull encoding/json into multiple
// other files for one liner each.
package proxy

import (
	"encoding/json"
	"io"
)

// decodeJSON parses a JSON body into v. Unknown fields are tolerated —
// HTTPResolver and ShareValidator share types with master; we don't
// want a master-side field addition to break the proxy.
func decodeJSON(r io.Reader, v any) error {
	return json.NewDecoder(r).Decode(v)
}
