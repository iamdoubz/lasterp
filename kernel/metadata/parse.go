// SPDX-License-Identifier: AGPL-3.0-only

package metadata

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// ParseObject parses and validates an Object schema definition
// (docs/03-DATA-MODEL.md's YAML shape).
func ParseObject(data []byte) (*Object, error) {
	var o Object
	if err := yaml.Unmarshal(data, &o); err != nil {
		return nil, fmt.Errorf("metadata: parse object: %w", err)
	}
	if err := o.Validate(); err != nil {
		return nil, err
	}
	return &o, nil
}
