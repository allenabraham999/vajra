package models

import (
	"encoding/json"
	"fmt"
)

// scanJSON unmarshals a JSON-encoded SQL column into dst. PostgreSQL's
// jsonb driver returns []byte; some other drivers return string. NULL
// columns are a no-op (dst is left at its zero value).
func scanJSON(src any, dst any) error {
	if src == nil {
		return nil
	}
	var b []byte
	switch s := src.(type) {
	case []byte:
		b = s
	case string:
		b = []byte(s)
	default:
		return fmt.Errorf("scanJSON: unsupported source type %T", src)
	}
	return json.Unmarshal(b, dst)
}
