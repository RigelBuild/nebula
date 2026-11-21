package jscompat

import "fmt"

// Lookup returns the raw value of name and whether it is non-blank after trimming.
// It treats a value that is empty or only whitespace after trimming as missing.
func Lookup(getenv func(string) string, name string) (string, bool) {
	value := getenv(name)
	return value, Trim(value) != ""
}

// Required returns the value of name, or an error when it is unset or blank after trimming.
func Required(getenv func(string) string, name string) (string, error) {
	value, ok := Lookup(getenv, name)
	if !ok {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}
