package defaults

import (
	"fmt"
	"os"
)

// GetPersona reads /config/persona.md fresh on every call — the opposite
// caching decision from Load's Table, because hot-editing the persona
// without a rebuild is a P5 exit criterion (G5) and the vocabulary table has
// no such requirement.
//
// A minimal persona.md draft landed in session 19 to resolve Q-16 (tone for an
// unanswered morning briefing); P5 expands it with the full tone ladder. A
// missing file is still not an error - a deployment can delete it - it means
// "no persona configured yet" and returns ("", nil) so a caller omits the
// section rather than injecting an empty heading. Any other read failure is
// returned so a caller can decide whether to log it and continue with "".
func GetPersona(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("defaults: read %s: %w", path, err)
	}
	return string(data), nil
}
