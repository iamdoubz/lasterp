package metadata

import (
	"errors"
	"testing"
)

const sampleContactYAML = `
object: Contact
module: crm
persistence: crud
fields:
  - {name: full_name, type: text, required: true}
  - {name: email, type: email}
  - {name: newsletter_opt_in, type: bool}
permissions:
  read: [crm.viewer]
  create: [crm.user]
  update: [crm.user]
  delete: [crm.admin]
`

func TestParseObjectValid(t *testing.T) {
	o, err := ParseObject([]byte(sampleContactYAML))
	if err != nil {
		t.Fatalf("ParseObject: %v", err)
	}
	if o.ObjectName != "Contact" {
		t.Fatalf("ObjectName = %q, want Contact", o.ObjectName)
	}
	if len(o.Fields) != 3 {
		t.Fatalf("got %d fields, want 3", len(o.Fields))
	}
}

func TestParseObjectRejectsUnknownFieldType(t *testing.T) {
	const bad = `
object: Bad
persistence: crud
fields:
  - {name: x, type: not_a_real_type}
`
	if _, err := ParseObject([]byte(bad)); !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("err = %v, want ErrInvalidObject", err)
	}
}

func TestParseObjectRejectsMissingRequiredAttrs(t *testing.T) {
	cases := map[string]string{
		"no object name":  "persistence: crud\nfields:\n  - {name: x, type: text}\n",
		"bad persistence": "object: X\npersistence: not_a_mode\nfields:\n  - {name: x, type: text}\n",
		"no fields":       "object: X\npersistence: crud\n",
		"duplicate field": "object: X\npersistence: crud\nfields:\n  - {name: x, type: text}\n  - {name: x, type: int}\n",
	}
	for name, yamlDoc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseObject([]byte(yamlDoc)); !errors.Is(err, ErrInvalidObject) {
				t.Fatalf("err = %v, want ErrInvalidObject", err)
			}
		})
	}
}
