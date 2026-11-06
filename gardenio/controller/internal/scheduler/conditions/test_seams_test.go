package conditions

import "encoding/json"

// MarshalExpression renders an Expression to JSON. Returned string is
// canonical (composition keys then leaf keys, omitempty applied) so
// round-trip parse/marshal yields equivalent semantic output.
func MarshalExpression(e Expression) (string, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
