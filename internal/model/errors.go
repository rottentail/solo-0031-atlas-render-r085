package model

import "fmt"

type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }
func Invalid(format string, args ...any) error {
	return &Error{400, "invalid", fmt.Sprintf(format, args...)}
}
func Conflict(message string) error { return &Error{409, "conflict", message} }
func Missing(message string) error  { return &Error{404, "missing", message} }
func Gone(message string) error     { return &Error{410, "gone", message} }
func Busy(message string) error     { return &Error{429, "capacity", message} }
func Unavailable(format string, args ...any) error {
	return &Error{503, "fenced", fmt.Sprintf(format, args...)}
}
