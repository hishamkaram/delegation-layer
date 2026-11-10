package ui

import (
	"strconv"

	"github.com/hishamkaram/delegation-layer/internal/app"
)

func TextField(label, hint, value string, apply func(*app.Arguments, string)) Field {
	return Field{Label: label, Hint: hint, Value: value, Apply: func(a *app.Arguments, value string) error { apply(a, value); return nil }}
}

func IntegerField(label string, value int64, apply func(*app.Arguments, int64)) Field {
	return Field{Label: label, Value: strconv.FormatInt(value, 10), Apply: func(a *app.Arguments, text string) error {
		parsed, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return err
		}
		apply(a, parsed)
		return nil
	}}
}
