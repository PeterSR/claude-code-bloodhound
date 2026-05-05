//go:build !unix

package guiapp

import (
	"context"
	"errors"
)

type FocusMessage struct {
	ActivationToken string `json:"activation_token,omitempty"`
}

var errUnsupported = errors.New("guiapp: bloodhound-gui is unix-only at the moment")

func Bootstrap() (bool, func(), error)             { return false, func() {}, errUnsupported }
func SendFocusToPrimary(ctx context.Context) error { return errUnsupported }
func ListenForFocus(onFocus func(FocusMessage)) error {
	return errUnsupported
}
