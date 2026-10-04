package main

import "testing"

// TestHelpExitsSuccessfully covers a crash and a nuisance that shipped together:
// `compare --help` panicked (the argument list was empty and the command indexed
// it), and `run --help` printed its usage and then reported a missing config.
//
// Asking for help is not a failure, so all three exit 0.
func TestHelpExitsSuccessfully(t *testing.T) {
	for _, args := range [][]string{
		{"run", "--help"},
		{"run", "-h"},
		{"capacity", "--help"},
		{"capacity", "-h"},
		{"compare", "--help"},
		{"compare", "-h"},
	} {
		t.Run(args[0]+" "+args[1], func(t *testing.T) {
			// The assertion is that this returns at all, and with a success.
			if code := run(args); code != exitPass {
				t.Errorf("exit = %d, want %d", code, exitPass)
			}
		})
	}
}

// TestHelpFlagIsNotReadAsAPath keeps `--help` from being taken for a result file
// now that a command takes positional arguments.
func TestHelpFlagIsNotReadAsAPath(t *testing.T) {
	if code := run([]string{"compare", "--help", "a.json"}); code != exitPass {
		t.Errorf("exit = %d, want %d", code, exitPass)
	}
}

// TestNoArgumentsIsAnError: the usage is printed, but nothing was asked for and
// nothing was done.
func TestNoArgumentsIsAnError(t *testing.T) {
	if code := run(nil); code != exitError {
		t.Errorf("exit = %d, want %d", code, exitError)
	}
}

// TestUnknownCommandIsAnError.
func TestUnknownCommandIsAnError(t *testing.T) {
	if code := run([]string{"frobnicate"}); code != exitError {
		t.Errorf("exit = %d, want %d", code, exitError)
	}
}
