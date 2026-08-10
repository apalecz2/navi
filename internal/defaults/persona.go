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
// persona.md does not exist until P5: this session builds the system
// prompt's part-2 slot, not the prose that fills it. A missing file is
// therefore not an error - it means "no persona configured yet" - and
// returns ("", nil) so a caller can omit the section entirely rather than
// injecting an empty heading. Any other read failure is returned so a caller
// can decide whether to log it and continue with "".
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
