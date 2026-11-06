package steward

// Default reports that the estimate is the configured default, not a mean.
func (e Estimate) Default() bool { return e.Sessions == 0 }
