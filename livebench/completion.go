package livebench

import "fmt"

// ValidateRunCompletion enforces the publication boundary for a full
// benchmark cell. Runners may continue after individual infrastructure errors
// so every sample gets an attempt and the failures remain inspectable, but a
// partial population must not advance to evaluation or be labeled complete.
func ValidateRunCompletion(planned, completed, failures int) error {
	if planned < 1 || completed < 0 || failures < 0 || completed > planned || failures > planned {
		return fmt.Errorf("invalid benchmark completion counts: planned=%d completed=%d failures=%d", planned, completed, failures)
	}
	if completed != planned || failures != 0 {
		return fmt.Errorf("benchmark population incomplete: planned=%d completed=%d failures=%d", planned, completed, failures)
	}
	return nil
}
