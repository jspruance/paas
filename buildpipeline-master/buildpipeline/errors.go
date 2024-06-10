package buildpipeline

import "errors"

var (
	// os errors
	ErrInvalid    = errors.New("invalid argument")
	ErrPermission = errors.New("permission denied")
	ErrExist      = errors.New("file already exists")
	ErrNotExist   = errors.New("file does not exist")
	ErrClosed     = errors.New("file already closed")

	// pipeline errors
	ErrCreateNamespace = errors.New("Error creating namespace")
	ErrTektonClientSet = errors.New("Error getting Tekton client set")
)

func errInvalid() error    { return ErrInvalid }
func errPermission() error { return ErrPermission }
func errExist() error      { return ErrExist }
func errNotExist() error   { return ErrNotExist }
func errClosed() error     { return ErrClosed }

func errCreateNamespace() error { return ErrCreateNamespace }
func errTektonClientSet() error { return ErrTektonClientSet }
