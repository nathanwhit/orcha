package model

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
)

// JSONMap is a free-form metadata bag persisted as a JSON column.
type JSONMap map[string]any

// Value implements driver.Valuer so JSONMap can be stored in SQLite.
func (m JSONMap) Value() (driver.Value, error) {
	if m == nil {
		return nil, nil
	}
	return json.Marshal(m)
}

// Scan implements sql.Scanner so JSONMap can be read from SQLite.
func (m *JSONMap) Scan(src any) error {
	if src == nil {
		*m = nil
		return nil
	}
	var b []byte
	switch v := src.(type) {
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		return fmt.Errorf("model: cannot scan %T into JSONMap", src)
	}
	if len(b) == 0 {
		*m = nil
		return nil
	}
	return json.Unmarshal(b, m)
}
