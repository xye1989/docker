package dockerfile

import "fmt"

// platformSupports is gives users a quality error message if a Dockerfile uses
// a command not supported on the platform.
func platformSupports(command string) error {
	switch command {
	case "user", "stopsignal", "arg":
		return fmt.Errorf("The daemon on this platform does not support the command '%s'", command)
	}
	return nil
}
