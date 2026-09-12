package surveybus

import "testing"

// TestBothSlashesCountAsSeparators is the Windows half of [ExcludesFor],
// asserted from inside because the decision it guards cannot be reached from
// a test on Linux: filepath.IsAbs answers differently on each platform, so the
// branch this feeds is only visibly wrong on a Windows runner. It was, once.
//
// Windows accepts a forward slash everywhere it accepts a backslash. Somebody
// who writes "C:/Users/jeff/Downloads" in the exclude box means the folder;
// testing only filepath.Separator there read it as a bare name and claimed it
// applied to every folder on the machine.
func TestBothSlashesCountAsSeparators(t *testing.T) {
	for pattern, want := range map[string]bool{
		`C:\Users\jeff\Downloads`: true,
		"C:/Users/jeff/Downloads": true,
		"/home/jeff/Downloads":    true,
		"node_modules":            false,
		"*.iso":                   false,
		"":                        false,
	} {
		if got := hasSeparator(pattern); got != want {
			t.Errorf("hasSeparator(%q) = %v, want %v", pattern, got, want)
		}
	}
}
