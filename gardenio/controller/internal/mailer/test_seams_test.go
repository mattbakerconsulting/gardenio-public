package mailer

// Kinds lists every Kind in the order the spec names them.
func Kinds() []Kind {
	return []Kind{
		KindApplicationReceived,
		KindApplicationApproved,
		KindApplicationDenied,
		KindApplicationQuestion,
		KindApplicationWithdrawn,
		KindLandReady,
		KindPurseLow,
		KindPurseEmpty,
		KindLandFrozen,
		KindLandClosed,
		KindLandJoined,
		KindInvite,
	}
}
