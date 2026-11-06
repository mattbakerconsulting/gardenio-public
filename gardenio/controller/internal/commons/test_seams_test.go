package commons

import "errors"

// IsNotFound reports whether err is Locate's answer for a bead with no place
// the viewer may see.
func IsNotFound(err error) bool { return errors.Is(err, errNotFound) }
