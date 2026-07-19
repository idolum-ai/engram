//go:build !linux && !darwin

package app

func readHostBootID() string { return "" }
