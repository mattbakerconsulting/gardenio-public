package gardenhub

// EmptyPrompt is the house voice (gd-5e6xgIOH6B): the placeholder, in the muted placeholder colour, of every empty box
// where a human types to the fleet. Not a feature and not instructions; state a composer needs to show ("next message
// wakes it") belongs in its hint line or button label, never here. It is the one copy: the beads viewer's templates read it
// through the emptyPrompt template func, and /garden carries it as <body data-empty-prompt> for its own composers.
const EmptyPrompt = "if you don't have any ideas, go outside"
