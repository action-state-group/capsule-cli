package cli

import "errors"

// pluginRequiredError is a command's fixed refusal when the thing it needs is
// not in this binary or not discovered. Its message is a static string carrying
// no path, secret, or profile detail -- like inputFileError and
// schemaLoadError, SafeError surfaces it in full instead of collapsing it to
// the generic ErrInput text, so the operator learns what is missing.
type pluginRequiredError struct{ message string }

func (e *pluginRequiredError) Error() string { return e.message }

func pluginRequiredErr(message string) error {
	return errors.Join(ErrInput, &pluginRequiredError{message: message})
}
