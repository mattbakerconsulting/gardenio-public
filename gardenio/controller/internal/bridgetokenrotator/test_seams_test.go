package bridgetokenrotator

// SetGenerateTokenFunc overrides the token generator for tests.
func (r *Rotator) SetGenerateTokenFunc(f func() (string, error)) {
	if f != nil {
		r.generateToken = f
	}
}
